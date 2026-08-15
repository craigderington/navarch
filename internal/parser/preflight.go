package parser

import (
	"gopkg.in/yaml.v3"
)

// preflight inspects the raw YAML document before compose-go loads it.
// include, extends, env_file and label_file must be rejected here: the
// loader reads those paths during Load, so a check after the fact still
// opens them. Profiles are also caught here because compose-go drops
// profiled services before convertService can see them.
func preflight(raw []byte) ValidationErrors {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		// Malformed YAML is compose-go's error to report.
		return nil
	}
	var errs ValidationErrors
	if _, ok := doc["include"]; ok {
		errs = append(errs, ValidationError{
			Field:   "include",
			Message: "include is not supported; the control plane does not read additional files",
		})
	}
	services, _ := doc["services"].(map[string]any)
	for name, rawSvc := range services {
		svc, ok := rawSvc.(map[string]any)
		if !ok {
			continue
		}
		field := func(f string) string { return "services." + name + "." + f }
		if _, ok := svc["extends"]; ok {
			errs = append(errs, ValidationError{
				Field:   field("extends"),
				Message: "extends is not supported; the control plane does not read additional files",
			})
		}
		if _, ok := svc["env_file"]; ok {
			errs = append(errs, ValidationError{
				Field:   field("env_file"),
				Message: "env_file is not supported; set environment inline or use ${secret:KEY}",
			})
		}
		if _, ok := svc["label_file"]; ok {
			errs = append(errs, ValidationError{
				Field:   field("label_file"),
				Message: "label_file is not supported; the control plane does not read additional files",
			})
		}
		if _, ok := svc["profiles"]; ok {
			errs = append(errs, ValidationError{
				Field:   field("profiles"),
				Message: "profiles are not supported; every service in the file is deployed",
			})
		}
	}
	return errs
}
