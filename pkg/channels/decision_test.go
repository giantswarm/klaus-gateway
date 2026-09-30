package channels

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

func TestDecision_Validate(t *testing.T) {
	now := time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC)
	valid := func() Decision {
		return Decision{
			Person:    "alex@example.com",
			Question:  "Roll tonight?",
			StatusQuo: "graveler runs it.",
			Options:   []store.DecisionOption{{Label: "Roll"}, {Label: "Wait", Consequence: "Monday."}},
			Recommend: 2,
			Due:       now.Add(time.Hour),
			Default:   "Wait.",
			AskedBy:   "the platform supervisor",
			Answer:    ToolInvocation{Tool: "x_beekeeper_note_answer"},
		}
	}
	require.NoError(t, valid().Validate(now))
	team := valid()
	team.Person, team.Team, team.Channel = "", "team-bumblebee", "C0123ABCDE"
	require.NoError(t, team.Validate(now))
	none := valid()
	none.Options, none.Recommend = nil, 0
	require.NoError(t, none.Validate(now), "a decision without options is answered in the person's own words")

	for name, mutate := range map[string]func(d *Decision){
		"person and team":         func(d *Decision) { d.Team, d.Channel = "team-bumblebee", "C0123ABCDE" },
		"neither":                 func(d *Decision) { d.Person = "" },
		"person without an email": func(d *Decision) { d.Person = "alex" },
		"person with a channel":   func(d *Decision) { d.Channel = "C0123ABCDE" },
		"team with a name":        func(d *Decision) { d.Person, d.Team, d.Channel = "", "team-bumblebee", "#bumblebee" },
		"no question":             func(d *Decision) { d.Question = " " },
		"two-line question":       func(d *Decision) { d.Question = "Roll?\nReally?" },
		"long question":           func(d *Decision) { d.Question = strings.Repeat("q", DecisionQuestionMax+1) },
		"no status quo":           func(d *Decision) { d.StatusQuo = "" },
		"eleven options": func(d *Decision) {
			d.Options = make([]store.DecisionOption, DecisionOptionsMax+1)
			for i := range d.Options {
				d.Options[i].Label = "o"
			}
		},
		"long label":           func(d *Decision) { d.Options[0].Label = strings.Repeat("l", DecisionLabelMax+1) },
		"empty label":          func(d *Decision) { d.Options[0].Label = "" },
		"recommends nothing":   func(d *Decision) { d.Recommend = 3 },
		"negative recommend":   func(d *Decision) { d.Recommend = -1 },
		"no due":               func(d *Decision) { d.Due = time.Time{} },
		"due now":              func(d *Decision) { d.Due = now },
		"no default":           func(d *Decision) { d.Default = "" },
		"no asker":             func(d *Decision) { d.AskedBy = "" },
		"no answer tool":       func(d *Decision) { d.Answer.Tool = "" },
		"long note identifier": func(d *Decision) { d.Note = strings.Repeat("n", DecisionNoteMax+1) },
	} {
		t.Run(name, func(t *testing.T) {
			d := valid()
			mutate(&d)
			require.Error(t, d.Validate(now))
		})
	}
}

func TestDecisionClose_Validate(t *testing.T) {
	for _, outcome := range []string{"answered", "defaulted", "withdrawn"} {
		require.NoError(t, DecisionClose{Outcome: outcome}.Validate())
	}
	require.Error(t, DecisionClose{Outcome: "done"}.Validate())
	require.Error(t, DecisionClose{Outcome: "answered", Text: strings.Repeat("t", DecisionAnswerMax+1)}.Validate())
}
