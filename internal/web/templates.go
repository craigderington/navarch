package web

import (
	"embed"
	"fmt"
	"html/template"
	"strings"
	"time"
)

// Embedded so the console is one binary with no runtime file dependency —
// the same property the CLI and the agent have, and the reason a container
// image of it needs nothing but the binary.
//
//go:embed templates/*.html
var templates embed.FS

// pages is one template set per page, each parsed from the layout plus that
// page alone.
//
// Not one set for everything: every page defines `content` and `title`, and Go
// templates share a single namespace, so parsing them together means the last
// file parsed silently wins and every page renders the same body. It fails
// quietly — the layout renders, the tables are simply empty — which is exactly
// the shape of bug this console exists to make visible in other systems.
// Every page that uses the layout must be listed. A missing entry is a
// runtime "no such template" and a blank page — which is why the demo loads
// each route rather than trusting this list to be complete.
var pages = []string{
	"fleet.html", "environments.html", "environment.html",
	"deployment.html", "events.html", "error.html", "confirm.html",
}

func parsePages() (map[string]*template.Template, error) {
	out := map[string]*template.Template{}
	for _, name := range pages {
		t, err := template.New(name).Funcs(funcs).ParseFS(templates, "templates/layout.html", "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		out[name] = t
	}
	// login.html and invite.html are standalone: both render before there is a
	// session, so neither can use a layout whose header shows who you are.
	for _, name := range []string{"login.html", "invite.html"} {
		t, err := template.New(name).Funcs(funcs).ParseFS(templates, "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		out[name] = t
	}
	return out, nil
}

var funcs = template.FuncMap{
	// ago renders an age, because "3m ago" is what an operator reads and an
	// RFC3339 timestamp is what they have to subtract in their head.
	"ago": func(v any) string {
		t, ok := asTime(v)
		if !ok {
			return "never"
		}
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return itoa(int(d.Minutes())) + "m ago"
		case d < 24*time.Hour:
			return itoa(int(d.Hours())) + "h ago"
		default:
			return itoa(int(d.Hours()/24)) + "d ago"
		}
	},
	// short truncates a uuid to the 8 characters the platform itself uses for
	// env8 and project names, so what the console shows matches what a
	// container is called.
	"short": func(s string) string {
		if len(s) > 8 {
			return s[:8]
		}
		return s
	},
	"pct": func(used, total int64) int {
		if total <= 0 {
			return 0
		}
		return int(used * 100 / total)
	},
	"pctI": func(used, total int) int {
		if total <= 0 {
			return 0
		}
		return used * 100 / total
	},
	"gib": func(b int64) string {
		return itoa(int(b/(1<<30))) + "G"
	},
	"upper":     strings.ToUpper,
	"hasPrefix": strings.HasPrefix,
	"dash": func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	},
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func asTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, !t.IsZero()
	case *time.Time:
		if t == nil || t.IsZero() {
			return time.Time{}, false
		}
		return *t, true
	case string:
		if t == "" {
			return time.Time{}, false
		}
		p, err := time.Parse(time.RFC3339, t)
		return p, err == nil
	}
	return time.Time{}, false
}
