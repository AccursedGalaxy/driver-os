package agent

import (
	"context"
	"sort"
	"strings"

	"github.com/AccursedGalaxy/driver-os/vcs"
)

// EvidenceStatus is the machine-readable state of one run guarantee. Its zero
// value means absent: the guarantee was neither configured nor attempted.
type EvidenceStatus string

const (
	EvidenceAbsent       EvidenceStatus = ""
	EvidenceSkipped      EvidenceStatus = "skipped"
	EvidenceDegraded     EvidenceStatus = "degraded"
	EvidenceInconclusive EvidenceStatus = "inconclusive"
	EvidenceFailed       EvidenceStatus = "failed"
	EvidencePassed       EvidenceStatus = "passed"
)

// DegradationReason is a closed enum. Consumers must switch on these values,
// rather than parsing the human Detail field.
type DegradationReason string

const (
	ReasonGateTimeout   DegradationReason = "gate-timeout"
	ReasonFailOpen      DegradationReason = "fail-open"
	ReasonUnpricedSpend DegradationReason = "unpriced-spend"
	ReasonNoVCS         DegradationReason = "no-vcs"
	ReasonPolicySkip    DegradationReason = "policy-skip"
	ReasonInfraFault    DegradationReason = "infra-fault"
	ReasonNotReached    DegradationReason = "not-reached"
	ReasonStaleEvidence DegradationReason = "stale-evidence"
	ReasonNoOpAnswer    DegradationReason = "no-op-answer"
)

// WorkspaceEffect records whether the final workspace tree differs from the
// run baseline. Unknown means the harness could not establish the fact.
type WorkspaceEffect string

const (
	WorkspaceChanged   WorkspaceEffect = "changed"
	WorkspaceUnchanged WorkspaceEffect = "unchanged"
	WorkspaceUnknown   WorkspaceEffect = "unknown"
)

// Degradation explains why a configured guarantee was weakened.
type Degradation struct {
	Guarantee string            `json:"guarantee"`
	Reason    DegradationReason `json:"reason"`
	Detail    string            `json:"detail,omitempty"`
}

// VerificationEvidence identifies what ran and which workspace generation it covered.
type VerificationEvidence struct {
	Status      EvidenceStatus `json:"status"`
	Command     string         `json:"command"`
	Attempts    int            `json:"attempts"`
	MutationGen int            `json:"mutation_gen"`
	FinalGen    int            `json:"final_gen"`
}

// DiffEvidence summarizes the captured workspace delta; patch content remains an artifact.
type DiffEvidence struct {
	Status          EvidenceStatus  `json:"status"`
	FilesChanged    []string        `json:"files_changed,omitempty"`
	PatchRef        string          `json:"patch_ref"`
	CaptureGen      int             `json:"capture_gen"`
	WorkspaceEffect WorkspaceEffect `json:"workspace_effect"`
}

// ReviewEvidence is a structured summary whose finding IDs refer to RunResult.Review.
type ReviewEvidence struct {
	Status              EvidenceStatus `json:"status"`
	Rounds              int            `json:"rounds"`
	FindingsTotal       int            `json:"findings_total"`
	BlockersFired       int            `json:"blockers_fired"`
	SurvivingFindingIDs []string       `json:"surviving_finding_ids,omitempty"`
	RepairedFindingIDs  []string       `json:"repaired_finding_ids,omitempty"`
	ReviewedGen         int            `json:"reviewed_gen"`
}

// Guarantees is the typed evidence report attached to every RunResult.
type Guarantees struct {
	Verification VerificationEvidence `json:"verification"`
	Review       ReviewEvidence       `json:"review"`
	CostBound    EvidenceStatus       `json:"cost_bound"`
	Isolation    EvidenceStatus       `json:"isolation"`
	Diff         DiffEvidence         `json:"diff"`
	Degradations []Degradation        `json:"degradations,omitempty"`
}

type verifyEvidenceEvent struct {
	status  EvidenceStatus
	command string
	gen     int
}

// evidenceLog is append-only run telemetry. mutGen deliberately over-approximates
// workspace mutation: every successful write_file/edit_file and every successful
// run/exec action bumps it, because arbitrary commands may alter the workspace.
type evidenceLog struct {
	verify                                             []verifyEvidenceEvent
	mutGen                                             int
	isolation                                          EvidenceStatus
	verifyConfigured, reviewConfigured, costConfigured bool
	verifyCommand                                      string
	closingReady                                       bool
	unpriced                                           bool
	root, baseTree                                     string
	reviewedGen                                        int
	reviewReached                                      bool
}

func (l *evidenceLog) mutation() {
	if l != nil {
		l.mutGen++
	}
}
func (l *evidenceLog) recordVerify(command string, status EvidenceStatus) {
	if l != nil {
		l.verify = append(l.verify, verifyEvidenceEvent{status: status, command: command, gen: l.mutGen})
	}
}
func (l *evidenceLog) degradation(g string, r DegradationReason, detail string, out *Guarantees) {
	out.Degradations = append(out.Degradations, Degradation{Guarantee: g, Reason: r, Detail: detail})
}

func finalizeGuarantees(l *evidenceLog, outcome Outcome, review *ReviewReport) Guarantees {
	var g Guarantees
	if l == nil {
		return g
	}
	g.Diff.WorkspaceEffect = WorkspaceUnknown
	g.Isolation = l.isolation
	g.CostBound = EvidencePassed
	if !l.costConfigured {
		g.CostBound = EvidenceAbsent
	}
	if l.unpriced {
		g.CostBound = EvidenceDegraded
		l.degradation("cost-bound", ReasonUnpricedSpend, "continued with unpriced spend", &g)
	}
	g.Verification.FinalGen = l.mutGen
	if l.verifyConfigured {
		g.Verification.Command = l.verifyCommand
		if len(l.verify) == 0 {
			g.Verification.Status = EvidenceSkipped
			l.degradation("verification", ReasonNotReached, "verification stage was not reached", &g)
		} else {
			last := l.verify[len(l.verify)-1]
			g.Verification.Status, g.Verification.Command, g.Verification.Attempts, g.Verification.MutationGen = last.status, last.command, len(l.verify), last.gen
			if last.gen < l.mutGen {
				g.Verification.Status = EvidenceDegraded
				l.degradation("verification", ReasonStaleEvidence, "workspace changed after verification", &g)
			}
		}
	}
	if l.reviewConfigured {
		g.Review.ReviewedGen = l.reviewedGen
		if review == nil || (!l.reviewReached && review.Rounds == 0) {
			g.Review.Status = EvidenceSkipped
			l.degradation("review", ReasonNotReached, "review stage was not reached", &g)
		} else {
			g.Review.Status = EvidencePassed
			g.Review.Rounds = review.Rounds
			g.Review.FindingsTotal = len(review.Findings)
			for _, f := range review.Findings {
				id := f.ID
				if strings.EqualFold(f.Severity, "blocker") {
					g.Review.BlockersFired++
				}
				if f.Fate == FateRepaired {
					g.Review.RepairedFindingIDs = append(g.Review.RepairedFindingIDs, id)
				} else if f.Fate == FateExpired {
					g.Review.SurvivingFindingIDs = append(g.Review.SurvivingFindingIDs, id)
				}
			}
			if review.Blocked {
				g.Review.Status = EvidenceFailed
			} else if review.Skipped != "" {
				g.Review.Status = EvidenceDegraded
				l.degradation("review", ReasonPolicySkip, review.Skipped, &g)
			}
			if l.reviewedGen < l.mutGen {
				g.Review.Status = EvidenceDegraded
				l.degradation("review", ReasonStaleEvidence, "workspace changed after review", &g)
			}
		}
	}
	if l.root != "" && l.baseTree != "" {
		ctx, cancel := context.WithTimeout(context.Background(), gateDiffTimeout)
		defer cancel()
		if cur, err := vcs.WriteTree(ctx, l.root); err == nil {
			if cur == l.baseTree {
				g.Diff.WorkspaceEffect = WorkspaceUnchanged
			} else {
				g.Diff.WorkspaceEffect = WorkspaceChanged
			}
			if names, e := vcs.DiffTreeNames(ctx, l.root, l.baseTree, cur); e == nil {
				sort.Strings(names)
				g.Diff.Status = EvidencePassed
				g.Diff.FilesChanged = names
				g.Diff.CaptureGen = l.mutGen
			} else {
				g.Diff.WorkspaceEffect = WorkspaceUnknown
				g.Diff.Status = EvidenceInconclusive
			}
		} else {
			g.Diff.Status = EvidenceInconclusive
		}
	} else if l.root != "" {
		g.Diff.Status = EvidenceSkipped
		l.degradation("diff", ReasonNoVCS, "no workspace baseline available", &g)
	}
	if outcome == Answered && g.Diff.WorkspaceEffect == WorkspaceUnchanged {
		l.degradation("diff", ReasonNoOpAnswer, "answered with no workspace changes", &g)
	}
	return g
}

func isMutatingTool(name string) bool {
	return name == "write_file" || name == "edit_file" || name == "run" || name == "exec"
}
