package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// The subset of scripts/deploychecklist's JSON (its SchemaVersion 1) this tool
// reads. Decoded strictly enough to refuse a schema it does not know: the
// checklist is the only thing that decides what is deployed, so a field that
// moved must stop the deploy rather than read as empty.
const checklistSchema = 1

const (
	statusChanged = "changed"
	kindCodeOnly  = "code-only"
)

type checklistBuild struct {
	GOOS    string   `json:"goos"`
	GOARCH  string   `json:"goarch"`
	Tags    []string `json:"tags,omitempty"`
	Command string   `json:"command"`
}

type checklistMechanism struct {
	Path    string   `json:"path"`
	Kind    string   `json:"kind"` // "sam" or "shell"
	Command string   `json:"command"`
	Files   []string `json:"files"`
}

type checklistLambda struct {
	ID         string               `json:"id"`
	Functions  []string             `json:"functions"`
	Build      checklistBuild       `json:"build"`
	Mechanisms []checklistMechanism `json:"mechanisms"`
	Status     string               `json:"status"`
	DeployKind string               `json:"deploy_kind"`
	Reasons    []string             `json:"reasons"`
}

type checklistInput struct {
	Path       string `json:"path"`
	Component  string `json:"component,omitempty"`
	Why        string `json:"why"`
	Command    string `json:"command"`
	Status     string `json:"status"`
	DeployKind string `json:"deploy_kind"`
}

type checklist struct {
	Schema     int               `json:"schema"`
	From       string            `json:"from"`
	To         string            `json:"to"`
	FromCommit string            `json:"from_commit"`
	ToCommit   string            `json:"to_commit"`
	Lambdas    []checklistLambda `json:"lambdas"`
	Inputs     []checklistInput  `json:"deploy_inputs"`
}

func readChecklist(path string) (*checklist, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c checklist
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Schema != checklistSchema {
		return nil, fmt.Errorf("%s: checklist schema %d, this deployer reads schema %d; update it with scripts/deploychecklist", path, c.Schema, checklistSchema)
	}
	if len(c.Lambdas) == 0 {
		return nil, fmt.Errorf("%s lists no Lambdas: the checklist discovery is broken, not the release", path)
	}
	return &c, nil
}

// target is one function to deploy.
type target struct {
	Lambda   checklistLambda
	Function string
}

// skip is a component, function or input this run does not deploy, and why.
type skip struct {
	What   string
	Status string
	Reason string
}

// selectTargets is the ONLY place that decides what gets deployed: a Lambda
// whose status is "changed" and whose deploy kind is "code-only". A Lambda whose
// own deploy mechanism changed is "manual", and an added or removed one is a
// create or a teardown, not a code update; all of those, the unchanged ones and
// every changed non-Lambda deploy input are listed as not deployed.
func selectTargets(c *checklist) ([]target, []skip) {
	var deploy []target
	var skipped []skip
	for _, l := range c.Lambdas {
		if l.Status == statusChanged && l.DeployKind == kindCodeOnly {
			for _, f := range l.Functions {
				deploy = append(deploy, target{Lambda: l, Function: f})
			}
			if len(l.Functions) == 0 {
				skipped = append(skipped, skip{What: l.ID, Status: l.Status + ", " + l.DeployKind,
					Reason: "manual, not deployed: the checklist names no function for it"})
			}
			continue
		}
		s := skip{What: l.ID, Status: l.Status + ", " + l.DeployKind}
		if fns := strings.Join(l.Functions, ", "); fns != "" {
			s.What += " (" + fns + ")"
		}
		switch {
		case l.Status == "unchanged":
			s.Reason = "unchanged, no deploy"
		default:
			s.Reason = "manual, not deployed"
			if len(l.Reasons) > 0 {
				s.Reason += ": " + strings.Join(l.Reasons, "; ")
			}
		}
		skipped = append(skipped, s)
	}
	for _, in := range c.Inputs {
		if in.Status == "unchanged" {
			continue
		}
		skipped = append(skipped, skip{What: in.Path, Status: in.Status + ", " + in.DeployKind,
			Reason: "manual, not deployed: " + in.Why})
	}
	return deploy, skipped
}
