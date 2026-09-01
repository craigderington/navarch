package rollout

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/craigderington/navarch/internal/mail"
	"github.com/craigderington/navarch/internal/store"
)

// TombstoneRetention is how long a teardown instruction is offered to agents.
// It doubles as how long a node may be offline and still clean up after itself:
// past it, that environment's pinned containers and named volumes leak and need
// manual removal.
const TombstoneRetention = 24 * time.Hour

// Reaper is the third control-plane loop. The scheduler places deployments and
// the controller drives them to live; the reaper is what ends them.
type Reaper struct {
	st    *store.Store
	log   *slog.Logger
	orgID *uuid.UUID
	// logs is the in-memory buffer holding delivered container output. The
	// reaper frees a request's buffer when it deletes its row, so memory cannot
	// outlive the instruction that justified holding it.
	logs   LogBuffer
	notify Notifier
}

// LogBuffer is the part of the control plane's log buffer the reaper needs.
// Named here rather than imported concretely so this loop stays testable
// without one, and so nothing tempts a future caller into reading content here.
type LogBuffer interface {
	Drop(id uuid.UUID)
	Expire(maxAge time.Duration) int
}

// WithLogBuffer attaches the buffer whose entries the reaper frees alongside the
// rows they belong to.
func (r *Reaper) WithLogBuffer(b LogBuffer) *Reaper { r.logs = b; return r }

// WithNotifier makes the reaper warn an organization's operators before it
// destroys a preview. Without it nothing is sent and the reaper behaves exactly
// as it did.
func (r *Reaper) WithNotifier(n Notifier) *Reaper { r.notify = n; return r }

// ExpiryWarning is how long before a preview's expiry its operators are told.
//
// One hour, against a default TTL of 24. Short enough that the warning is about
// something imminent rather than something to forget, long enough to extend the
// work by redeploying if the branch is still open. A preview created with a TTL
// shorter than this is warned about almost immediately, which is not a bug: a
// preview that lives under an hour is one whose expiry is genuinely news.
//
// Not configurable, deliberately. TTL is already the lifecycle control and the
// only one — see the preview invariants — and a second knob governing when you
// hear about the first is a setting nobody would ever have a reason to change.
const ExpiryWarning = time.Hour

// LogBufferIdleTTL frees a buffer nobody has read or written for this long. A
// requester that walks away mid-tail leaves no row transition behind, so idleness
// is the only signal its memory is dead weight.
const LogBufferIdleTTL = 10 * time.Minute

// SecretVersionRetention is how long a superseded secret version outlives its
// replacement. Rotation does not narrow who can open history — old ciphertext
// stays sealed to whatever recipients were live at write time — so the reaper
// retires it. Long enough to be evidence in an incident, short enough that
// "who could read last month's password" has an end date.
const SecretVersionRetention = 30 * 24 * time.Hour

func newReaperForOrg(st *store.Store, log *slog.Logger, orgID uuid.UUID) *Reaper {
	return &Reaper{st: st, log: log, orgID: &orgID}
}

func NewReaper(st *store.Store, log *slog.Logger) *Reaper {
	return &Reaper{st: st, log: log}
}

// ReapOnce deletes expired preview environments and drops teardown instructions
// no agent will act on any more.
func (r *Reaper) ReapOnce(ctx context.Context) error {
	var reaped []string
	var err error
	if r.orgID == nil {
		reaped, err = r.st.ExpireEnvironments(ctx)
	} else {
		reaped, err = r.st.ExpireEnvironmentsForOrg(ctx, *r.orgID)
	}
	if err != nil {
		return err
	}
	for _, env8 := range reaped {
		r.log.Info("preview environment expired", "env", env8)
	}
	// Warn about the ones that are close, after deleting the ones that are gone.
	// Claim-and-mark is atomic in the store, so this cannot re-send on the next
	// tick a second later.
	r.warnExpiring(ctx)
	// Expired log instructions, and the memory their answers occupy. Both, and
	// in that order: deleting the rows while leaving the buffers would keep
	// container output — possibly containing secrets — alive in the control
	// plane with nothing left pointing at it.
	swept, err := r.st.SweepLogRequests(ctx)
	if err != nil {
		return err
	}
	if r.logs != nil {
		for _, id := range swept {
			r.logs.Drop(id)
		}
		r.logs.Expire(LogBufferIdleTTL)
	}
	// Superseded secret versions past retention. Old ciphertext stays sealed
	// to the recipients live at write time, so keeping it forever keeps keys
	// nobody meant to keep. Runs after the log sweep for the same reason as
	// everything else in this loop: deleting durable state is the reaper's
	// job, and doing it here keeps every "when does data disappear" answer
	// in one place.
	var pruned int64
	if r.orgID == nil {
		pruned, err = r.st.PruneSecretVersions(ctx, SecretVersionRetention)
	} else {
		pruned, err = r.st.PruneSecretVersionsForOrg(ctx, *r.orgID, SecretVersionRetention)
	}
	if err != nil {
		return err
	}
	if pruned > 0 {
		r.log.Info("pruned superseded secret versions", "count", pruned)
	}
	if r.orgID == nil {
		return r.st.SweepTombstones(ctx, TombstoneRetention)
	}
	return r.st.SweepTombstonesForOrg(ctx, *r.orgID, TombstoneRetention)
}

// warnExpiring emails operators about previews the reaper is about to destroy.
//
// Every error is a log line and nothing more. The reaper's job is to end
// environments on time, and a mail provider being unreachable must not stop it
// — an environment that outlived its TTL because an email failed would be the
// tail wagging the dog, and TTL is the only lifecycle control previews have.
func (r *Reaper) warnExpiring(ctx context.Context) {
	if r.notify == nil {
		return
	}
	var due []store.ExpiringPreview
	var err error
	if r.orgID == nil {
		due, err = r.st.ClaimPreviewsForExpiryWarning(ctx, ExpiryWarning)
	} else {
		due, err = r.st.ClaimPreviewsForExpiryWarningForOrg(ctx, ExpiryWarning, *r.orgID)
	}
	if err != nil {
		r.log.Warn("could not claim previews for expiry warning", "error", err)
		return
	}
	for _, p := range due {
		target, err := r.st.NotifyTargetsForEnvironment(ctx, p.ID)
		if err != nil {
			r.log.Warn("could not resolve expiry warning recipients", "environment", p.ID, "error", err)
			continue
		}
		if len(target.Emails) == 0 {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = r.notify.Send(sendCtx, expiryMessage(target, p))
		cancel()
		if err != nil {
			// The row is already marked, so this warning is lost rather than
			// retried. Said plainly in the log, because the alternative — a
			// retry that re-sends every tick — is the failure that trains people
			// to filter the sender.
			r.log.Warn("expiry warning not delivered; it will not be retried",
				"environment", p.ID, "error", err)
			continue
		}
		r.log.Info("preview expiry warning sent", "environment", p.ID, "recipients", len(target.Emails))
	}
}

func expiryMessage(t *store.NotifyTarget, p store.ExpiringPreview) mail.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "The preview environment %s expires at %s.\n\n",
		t.Path(), p.ExpiresAt.UTC().Format(time.RFC3339))
	if t.Hostname != "" {
		fmt.Fprintf(&b, "  hostname     %s\n", t.Hostname)
	}
	fmt.Fprintf(&b, "  environment  %s\n\n", t.Path())
	// Say what expiry actually does. "Expires" could mean the route stops or
	// the containers stop; here it means the durable state is destroyed, and an
	// operator who assumed the weaker meaning would not act in time.
	b.WriteString("When it expires the environment is destroyed: its containers, its\n")
	b.WriteString("pinned services and its named volumes, all of it. Preview data is not\n")
	b.WriteString("backed up and is not recoverable afterwards.\n\n")
	b.WriteString("TTL is the only lifecycle control a preview has — there is no extend.\n")
	b.WriteString("If the work is still open, create a new preview.\n")

	return mail.Message{
		To:      t.Emails,
		Subject: fmt.Sprintf("[navarch] preview expiring in under %s: %s", ExpiryWarning, t.Path()),
		Body:    b.String(),
	}
}
