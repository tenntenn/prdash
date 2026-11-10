package prdash_test

import (
	"strconv"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

func TestHumanHandoffPreservesTechnicalState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ci        string
		conflicts bool
	}{
		{"failing", true},
		{"running", true},
		{"passing", true},
		{"failing", false},
		{"running", false},
	}
	for _, tc := range cases {
		t.Run(tc.ci+"/"+strconv.FormatBool(tc.conflicts), func(t *testing.T) {
			t.Parallel()
			svc := newSeededService(t)
			pr, err := svc.GetPR(101)
			if err != nil {
				t.Fatal(err)
			}
			pr.CIStatus = tc.ci
			pr.HasConflicts = tc.conflicts
			pr, err = svc.UpsertPR(pr)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.ReportSessionResult(pr.SessionID, pr.Number, prdash.StatusWaitingForHuman, "sample-clarification-needed")
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != prdash.StatusWaitingForHuman || got.CIStatus != pr.CIStatus || got.HasConflicts != pr.HasConflicts || got.ReviewStatus != pr.ReviewStatus {
				t.Fatalf("handoff changed technical state: before=%+v after=%+v", pr, got)
			}
		})
	}
}
