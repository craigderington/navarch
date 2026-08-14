package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeploymentTimelineAndCursor(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)
	if err := st.UpdateDeploymentState(testCtx(t), dep.ID, DeployScheduling, ""); err != nil {
		t.Fatalf("transition: %v", err)
	}

	events, err := st.ListEvents(testCtx(t), node.OrgID, 0, 50)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) < 2 || events[0].Kind != "deployment.state_changed" || events[1].Kind != "deployment.created" {
		t.Fatalf("unexpected timeline: %+v", events)
	}
	if events[0].DeploymentID == nil || *events[0].DeploymentID != dep.ID {
		t.Fatalf("state event not linked to deployment: %+v", events[0])
	}

	older, err := st.ListEvents(testCtx(t), node.OrgID, events[0].ID, 1)
	if err != nil {
		t.Fatalf("cursor ListEvents: %v", err)
	}
	if len(older) != 1 || older[0].ID >= events[0].ID {
		t.Fatalf("cursor did not return older event: %+v", older)
	}
}

func TestSecretAuditPayloadNeverContainsValue(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)
	const plaintext = "never-record-this"
	if err := st.SetSecret(testCtx(t), dep.EnvironmentID, "api_key", []byte(plaintext), "recipient"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	events, err := st.ListEvents(testCtx(t), node.OrgID, 0, 1)
	if err != nil || len(events) != 1 {
		t.Fatalf("ListEvents: events=%+v err=%v", events, err)
	}
	if events[0].Kind != "secret.set" || events[0].Payload["key"] != "api_key" {
		t.Fatalf("unexpected secret event: %+v", events[0])
	}
	payload, _ := json.Marshal(events[0].Payload)
	if strings.Contains(string(payload), plaintext) {
		t.Fatal("secret plaintext appeared in audit payload")
	}
}
