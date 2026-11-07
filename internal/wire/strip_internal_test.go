package wire

import (
	"strings"
	"testing"
)

// TestStripJudgeSeverity exercises the helper directly: it strips the CVSS-assignment
// wording when the anchors match, and — the important safety net — returns ok=false
// when a customized judge prompt no longer contains them, so Build falls back to the
// appended override instead of silently leaving the rating instruction in place.
func TestStripJudgeSeverity(t *testing.T) {
	// The exact passages from assets/exploiter/agents/judge.md that the helper targets.
	const anchored = "intro paragraph\n\n" +
		"5. **Assign a CVSS-based severity: CRITICAL / HIGH / MEDIUM / LOW.** Rate the exploit's real-world\n" +
		"   severity in line with CVSS reasoning (impact on confidentiality/integrity/availability, attack\n" +
		"   vector, complexity, privileges and user interaction required). **An exploit that only works under\n" +
		"   unrealistic conditions or in rare/atypical deployments must not be rated CRITICAL** — the rating\n" +
		"   must reflect how exploitable real deployments actually are, not the best-case lab scenario. Briefly\n" +
		"   justify the rating (you may cite an approximate CVSS vector/score if helpful).\n\n" +
		"- the **severity: CRITICAL / HIGH / MEDIUM / LOW**, with a brief CVSS-based justification;\n"

	out, ok := stripJudgeSeverity(anchored)
	if !ok {
		t.Fatal("anchors did not match the known judge wording — the asset and the helper have drifted")
	}
	if strings.Contains(out, "Assign a CVSS-based severity") {
		t.Error("the CVSS-assignment step was not stripped")
	}
	if strings.Contains(out, "with a brief CVSS-based justification") {
		t.Error("the severity return bullet was not rewritten")
	}
	if !strings.Contains(out, "do NOT assign a severity rating yourself") {
		t.Error("the replacement instruction is missing")
	}
	if !strings.Contains(out, "automated decision model assigns the") {
		t.Error("the replacement should say the decision model assigns the rating")
	}

	// Fallback: a customized judge prompt with none of the anchors returns ok=false.
	if _, ok := stripJudgeSeverity("# Judge\nA fully customized prompt that never mentions a rating step."); ok {
		t.Error("expected ok=false when the strip anchors are absent, so Build can fall back to the override")
	}
}
