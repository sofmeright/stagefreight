package commit

import "fmt"

// StageMode determines how files are staged before commit.
type StageMode string

const (
	StageExplicit StageMode = "explicit" // --add paths
	StageAll      StageMode = "all"      // --all (git add -A)
	StageStaged   StageMode = "staged"   // default: commit whatever is already staged
)

// PushOptions controls post-commit push behavior.
type PushOptions struct {
	Enabled         bool
	Remote          string // default: "origin"
	Refspec         string // default: "" (current branch)
	RebaseOnDiverge bool   // when true (default), rebase onto upstream if diverged before pushing
}

// Plan is a fully resolved, validated commit intent.
type Plan struct {
	Type      string
	Scope     string
	Summary   string
	Body      string
	Breaking  bool
	SkipCI    bool
	Paths     []string // for StageExplicit
	StageMode StageMode
	Push      PushOptions
	SignOff   bool
}

// Subject renders the commit subject line.
// When conventional is true: {type}[({scope})][!]: {summary}[ [skip ci]]
// When conventional is false: {summary}[ [skip ci]]
func (p Plan) Subject(conventional bool) string {
	var subject string
	if conventional {
		subject = p.Type
		if p.Scope != "" {
			subject += fmt.Sprintf("(%s)", p.Scope)
		}
		if p.Breaking {
			subject += "!"
		}
		subject += ": " + p.Summary
	} else {
		subject = p.Summary
	}
	if p.SkipCI {
		subject += " [skip ci]"
	}
	return subject
}

// Message renders the full commit message (subject + optional body).
// The SF-generated trailer is always appended so that the replay gate
// can identify and safely rebase these commits.
func (p Plan) Message(conventional bool) string {
	msg := p.Subject(conventional)
	if p.Body != "" {
		msg += "\n\n" + p.Body
	}
	msg += "\n\n" + sfGeneratedTrailer
	return msg
}
