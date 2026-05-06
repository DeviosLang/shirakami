// Package benchmark — AutoJudgeExtra: adversarial three-phase verdict for
// shadow-parity extra_pending records.
//
// Architecture §12.6: Generate → Critic → Judge
//
// Each extra_pending DiffRecord undergoes three LLM calls:
//   1. Proof   (Agent A): argue the call EXISTS, cite file+line
//   2. Disproof (Agent B): argue the call does NOT exist, cite reasons
//   3. Judge   (Arbiter): weigh both sides → EXISTS / NOT_EXISTS / UNCERTAIN
//
// Records judged UNCERTAIN remain extra_pending for human review.
// Typical shadow run produces 1-5 extras → 3-15 additional LLM calls.
package benchmark

import (
	"context"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// LLMClient interface
// ---------------------------------------------------------------------------

// LLMClient is the minimal interface AutoJudgeExtra needs from an LLM backend.
// It is satisfied by *llm.Client (internal/llm) without creating a circular
// import — callers pass the concrete client through this interface.
type LLMClient interface {
	// Complete sends a single-turn conversation and returns the assistant text.
	// Pass nil for tools when no tool use is needed.
	CompleteText(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// ---------------------------------------------------------------------------
// AutoJudgeExtra
// ---------------------------------------------------------------------------

// JudgeResult captures the outcome of AutoJudgeExtra for one record.
type JudgeResult struct {
	Record     DiffRecord
	NewCat     DiffCategory
	Proof      string // Agent A reasoning
	Disproof   string // Agent B reasoning
	JudgeRaw   string // raw Judge response
}

// AutoJudgeExtra runs the three-phase adversarial verification on a single
// extra_pending DiffRecord and returns the resolved DiffCategory.
//
// codeContext may include relevant source snippets to ground the LLM —
// pass an empty string when no context is available (LLM uses prior knowledge only).
//
// Returns one of:
//   - CategoryExtraTP  — call verified as real
//   - CategoryExtraFP  — call verified as spurious
//   - CategoryExtraPend — uncertain, keep for human review
func AutoJudgeExtra(ctx context.Context, llm LLMClient, record DiffRecord, codeContext string) (JudgeResult, error) {
	if record.Category != CategoryExtraPend {
		return JudgeResult{Record: record, NewCat: record.Category}, nil
	}

	caller := record.Edge.SourceFunc
	callerRepo := record.Edge.SourceRepo
	callerFile := record.Edge.SourceFile
	target := record.Edge.TargetFunc
	targetRepo := record.Edge.TargetRepo
	edgeType := record.Edge.EdgeType

	ctxNote := ""
	if codeContext != "" {
		ctxNote = "\n\nRelevant code context:\n```\n" + codeContext + "\n```"
	}

	// ── Phase 1: Proof (Agent A — argue call EXISTS) ─────────────────────────
	proofSystem := `You are a code analyst verifying whether a function call relationship exists in source code.
Be concrete: cite file paths, line numbers, and the exact call expression if you can find evidence.
If you cannot find concrete evidence, say exactly: NO EVIDENCE`

	proofUser := fmt.Sprintf(
		`Does the function "%s" (in repo "%s", file "%s") %s "%s" (in repo "%s")?

Argue that this relationship EXISTS. Provide:
- File path and approximate line number of the call site
- The exact call expression
- Why this is a real call and not a false match%s`,
		caller, callerRepo, callerFile, edgeType, target, targetRepo, ctxNote,
	)

	proof, err := llm.CompleteText(ctx, proofSystem, proofUser)
	if err != nil {
		return JudgeResult{}, fmt.Errorf("proof phase: %w", err)
	}

	// ── Phase 2: Disproof (Agent B — argue call does NOT exist) ──────────────
	disproofSystem := `You are a skeptical code reviewer checking for false positives in a call graph.
Your job is to find reasons why a reported function call might NOT actually exist.
Consider: regex false matches, comments, string literals, dead code, name shadowing, different packages.`

	disproofUser := fmt.Sprintf(
		`A call graph tool reports that "%s" (repo "%s", file "%s") %s "%s" (repo "%s").
An analyst argues it exists based on:
%s

Argue that this relationship does NOT actually exist. Consider:
- Is the match from a comment, string, or dead code branch?
- Could this be a different function with the same name in another package?
- Is there any indirection (interface, function pointer) that makes this uncertain?
- Is the evidence above conclusive or just circumstantial?%s`,
		caller, callerRepo, callerFile, edgeType, target, targetRepo, proof, ctxNote,
	)

	disproof, err := llm.CompleteText(ctx, disproofSystem, disproofUser)
	if err != nil {
		return JudgeResult{}, fmt.Errorf("disproof phase: %w", err)
	}

	// ── Phase 3: Judge (Arbiter — weigh both sides) ──────────────────────────
	judgeSystem := `You are an impartial judge evaluating conflicting evidence about a code call relationship.
Respond with exactly ONE of these three words on the first line, followed by a brief explanation:
  EXISTS       — the call relationship is real
  NOT_EXISTS   — the call relationship is spurious / false positive
  UNCERTAIN    — the evidence is inconclusive; human review needed`

	judgeUser := fmt.Sprintf(
		`Evaluate whether "%s" %s "%s".

Evidence FOR (call exists):
%s

Evidence AGAINST (call does not exist):
%s

Verdict (EXISTS / NOT_EXISTS / UNCERTAIN):`,
		caller, edgeType, target, proof, disproof,
	)

	judgeRaw, err := llm.CompleteText(ctx, judgeSystem, judgeUser)
	if err != nil {
		return JudgeResult{}, fmt.Errorf("judge phase: %w", err)
	}

	newCat := parseJudgeVerdict(judgeRaw)

	return JudgeResult{
		Record:   record,
		NewCat:   newCat,
		Proof:    proof,
		Disproof: disproof,
		JudgeRaw: judgeRaw,
	}, nil
}

// parseJudgeVerdict extracts the verdict from the Judge's raw response.
// It looks for the first word on the first non-empty line.
func parseJudgeVerdict(raw string) DiffCategory {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EXISTS"):
			return CategoryExtraTP
		case strings.HasPrefix(upper, "NOT_EXISTS"):
			return CategoryExtraFP
		default:
			return CategoryExtraPend
		}
	}
	return CategoryExtraPend
}

// ---------------------------------------------------------------------------
// RunAutoJudge — batch-process all extra_pending in a report
// ---------------------------------------------------------------------------

// AutoJudgeOptions controls the batch auto-judge run.
type AutoJudgeOptions struct {
	// CodeContext is optional source code snippets to inject into each prompt.
	// Keyed by edge key (DiffRecord.Edge.Key()); falls back to "" if not found.
	CodeContext map[string]string

	// MaxRecords caps how many extra_pending records are processed in one run.
	// 0 = no limit.
	MaxRecords int

	// Verbose causes each judgment to be printed to the provided writer.
	// nil = silent.
	Verbose func(format string, args ...any)
}

// AutoJudgeReport summarises the result of a RunAutoJudge call.
type AutoJudgeReport struct {
	Total      int // extra_pending records found
	Judged     int // records that received a definitive verdict (tp or fp)
	Remaining  int // records still pending (uncertain or skipped due to MaxRecords)
	TPCount    int
	FPCount    int
	Results    []JudgeResult
}

// RunAutoJudge iterates over all extra_pending records in report, calls
// AutoJudgeExtra for each, applies the verdict in-place, and returns a summary.
//
// The report is mutated: category fields are updated and counts are recalculated
// after all judgments. Call SaveReport afterwards to persist.
func RunAutoJudge(ctx context.Context, llm LLMClient, report *ParityReport, opts AutoJudgeOptions) (AutoJudgeReport, error) {
	var summary AutoJudgeReport

	verbose := opts.Verbose
	if verbose == nil {
		verbose = func(string, ...any) {}
	}

	processed := 0
	for i := range report.Records {
		r := &report.Records[i]
		if r.Category != CategoryExtraPend {
			continue
		}
		summary.Total++

		if opts.MaxRecords > 0 && processed >= opts.MaxRecords {
			summary.Remaining++
			continue
		}
		processed++

		codeCtx := ""
		if opts.CodeContext != nil {
			codeCtx = opts.CodeContext[r.Edge.Key()]
		}

		verbose("  auto-judging: %s:%s → %s ...", r.Edge.SourceRepo, r.Edge.SourceFunc, r.Edge.TargetFunc)

		result, err := AutoJudgeExtra(ctx, llm, *r, codeCtx)
		if err != nil {
			// Non-fatal: keep record as pending and log the error.
			verbose("  ERROR judging record: %v", err)
			summary.Remaining++
			summary.Results = append(summary.Results, JudgeResult{Record: *r, NewCat: CategoryExtraPend})
			continue
		}

		r.Category = result.NewCat
		switch result.NewCat {
		case CategoryExtraTP:
			r.Details = "auto-judged: tp (EXISTS)"
			summary.TPCount++
			summary.Judged++
			verbose("  → tp (EXISTS)")
		case CategoryExtraFP:
			r.Details = "auto-judged: fp (NOT_EXISTS)"
			summary.FPCount++
			summary.Judged++
			verbose("  → fp (NOT_EXISTS)")
		default:
			r.Details = "auto-judged: uncertain — pending human review"
			summary.Remaining++
			verbose("  → uncertain (pending)")
		}
		summary.Results = append(summary.Results, result)
	}

	// Recalculate aggregate counts and rates.
	report.MatchCount = 0
	report.MissCount = 0
	report.ExtraPendingCount = 0
	report.ExtraTPCount = 0
	report.ExtraFPCount = 0
	for _, rec := range report.Records {
		switch rec.Category {
		case CategoryMatch:
			report.MatchCount++
		case CategoryMiss:
			report.MissCount++
		case CategoryExtraPend:
			report.ExtraPendingCount++
		case CategoryExtraTP:
			report.ExtraTPCount++
		case CategoryExtraFP:
			report.ExtraFPCount++
		}
	}
	denom := report.MatchCount + report.MissCount
	if denom > 0 {
		report.MissRate = float64(report.MissCount) / float64(denom)
	}
	fpDenom := report.MatchCount + report.MissCount + report.ExtraFPCount
	if fpDenom > 0 {
		report.FPRate = float64(report.ExtraFPCount) / float64(fpDenom)
	}

	return summary, nil
}
