package core

import (
	"strings"
	"testing"

	"github.com/irfan/orion/internal/model"
)

// --- Audit 2: instruction-phrase word boundaries -------------------------

// TestDeclarativeSentencesWordBoundaries: instruction phrases are
// matched on word boundaries, not as raw substrings — a substring
// match drops legitimate fact sentences ("write" matches "writer",
// "compute" matches "computer").
func TestDeclarativeSentencesWordBoundaries(t *testing.T) {
	kept := []string{
		"My favourite writer is Asimov.",
		"My computer runs Linux.",
		"I abuse the system daily.",
	}
	for _, s := range kept {
		got := declarativeSentences(s)
		if len(got) != 1 || got[0] != s {
			t.Errorf("declarativeSentences(%q) = %v, want the sentence kept", s, got)
		}
	}
	dropped := []string{
		"Write a poem about my favourite number.",
		"Compute my favourite number times 3.",
		"Use the calculator to add 2 and 2.",
		"Tell me my favourite number.",
		"Do not delete anything.",
	}
	for _, s := range dropped {
		if got := declarativeSentences(s); len(got) != 0 {
			t.Errorf("declarativeSentences(%q) = %v, want the instruction dropped", s, got)
		}
	}
}

// TestRecallItemsRecallsWriterSentence: the word-boundary fix at the
// recall level — a fact stated with "writer" must surface.
func TestRecallItemsRecallsWriterSentence(t *testing.T) {
	older := []model.Message{{Role: "user", Content: "My favourite writer is Asimov."}}
	got := recallItems("Who is my favourite writer?", nil, older, nil)
	if len(got) != 1 || !strings.Contains(got[0], "Asimov") {
		t.Errorf("recallItems(writer sentence) = %v, want the Asimov sentence", got)
	}
}
