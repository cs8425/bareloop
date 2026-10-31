// Package compact implements the threshold-target compaction policy.
//
// It is deliberately a thin, mostly pure layer:
//
//	ShouldTrigger  §1.3  when compaction may even be considered
//	Plan           §2.1  §2.3  where to cut, using real per-message tokens
//	SummaryRequest §3.1 §3.5 §5.2 §5.3  how the summary call is built (prefix-stable)
//	Accepts        §3.4  whether the summary is small enough
//	Commit         §3.8  how the result is written (entry + tip in one step)
//
// Everything that touches storage lives in Commit, and everything that renders a payload
// lives in package asm — this package never assembles messages itself.
package compact

import (
	"encoding/json"
	"fmt"
	"strings"

	"bareloop/internal/asm"
	"bareloop/internal/ir"
	"bareloop/internal/llm"
)

// Summary payload shapes (§3.1).
const (
	// ModePrefixStable sends the last payload actually sent, byte-identical, plus one
	// instruction message. Default, and the only shape with KV cache affinity.
	ModePrefixStable = "prefix-stable"
	// ModeTranscript sends [system: instruction][user: flat transcript]; the fallback for
	// servers that cannot accept the prefix-stable shape.
	ModeTranscript = "transcript"
)

// DefaultMarker is written into the summary entry's content when the entry is created —
// never inside the assembler, which must stay WYSIWYG (§3.6).
const DefaultMarker = "[Conversation summary — earlier messages were compacted]"

// Params are the user-facing compaction parameters (§7).
type Params struct {
	// Auto is the master switch (T1) and defaults to off (spec §1.1). InLoop enables T2
	// (between agentic turns) and defaults to ON: a nil pointer means the key was omitted,
	// and only an explicit false turns it off.
	Auto   bool  `json:"auto"`
	InLoop *bool `json:"inLoop,omitempty"`
	// Mode is the policy for future cut decisions; compaction entries are mode-agnostic (§2.6).
	Mode string `json:"mode"`
	// SummaryMode selects the summary request shape (§3.1).
	SummaryMode string `json:"summaryMode"`
	// ThresholdPct is the trigger point and the cap on the total size after compaction.
	ThresholdPct int `json:"threshold"`
	// KeepTargetPct is the cap on the summary itself (acceptance check).
	KeepTargetPct int `json:"keepTarget"`
	// MinKeptUserTurns is a floor on boundary cuts (phase 1) and MinKeptMessages is a floor on the
	// kept tail itself, raising the cap when the two contradict each other (§2.1). Both mean "no floor"
	// at 0 and clamp negatives to 0 (see Normalise); the fresh-session default of 4 turns belongs to
	// the defaults, not to the field.
	MinKeptUserTurns int `json:"minKeptUserTurns"`
	MinKeptMessages  int `json:"minKeptMessages"`
	// MinSummarizeTokens: below this, a summary call is not worth it.
	MinSummarizeTokens int `json:"minSummarizeTokens"`
	// SummaryMaxPct is max_tokens of the summary call as a share of n_ctx.
	SummaryMaxPct int `json:"summaryMax"`
	// MaxAttempts counts total tries for the summary call (1 = single call, <=0 = forever).
	MaxAttempts int `json:"maxAttempts"`
	// Temperature of the summary call.
	Temperature float64 `json:"temperature"`
	// Marker prefixes every summary entry's content.
	Marker string `json:"marker"`
	// Instruction overrides the default summary instruction; SplitInstruction overrides the
	// "cut inside a turn" variant.
	Instruction      string `json:"instruction"`
	SplitInstruction string `json:"splitInstruction"`
}

// DefaultParams returns the documented defaults (§7): auto compaction off, in-loop compaction
// on (InLoop stays nil, which means "on unless explicitly disabled").
func DefaultParams() Params {
	return Params{
		Auto:               false,
		Mode:               "threshold-target",
		SummaryMode:        ModePrefixStable,
		ThresholdPct:       70,
		KeepTargetPct:      50,
		MinKeptUserTurns:   4,
		MinKeptMessages:    0,
		MinSummarizeTokens: 2000,
		SummaryMaxPct:      70,
		MaxAttempts:        3,
		Temperature:        0.2,
		Marker:             DefaultMarker,
	}
}

// UseInLoop is the effective value of InLoop: on unless explicitly disabled (T2, §1.1).
func (p Params) UseInLoop() bool { return p.InLoop == nil || *p.InLoop }

// Normalise fills in blanks and clamps nonsense, by one rule with two halves. A field whose 0 would
// be destructive (threshold, keepTarget, summaryMax, minSummarizeTokens) or is not a real answer
// (maxAttempts) falls back to its default at 0 and below. A field whose 0 IS the answer — the two
// floors minKeptUserTurns and minKeptMessages — keeps 0 as "no floor" and clamps only negatives to
// it, because promoting a negative to the shipped default would enforce a floor nobody asked for.
// The one documented exception is maxAttempts, where a negative means "retry forever" and survives.
func (p Params) Normalise() Params {
	d := DefaultParams()
	if p.ThresholdPct <= 0 {
		p.ThresholdPct = d.ThresholdPct
	}
	if p.KeepTargetPct <= 0 {
		p.KeepTargetPct = d.KeepTargetPct
	}
	if p.MinKeptUserTurns < 0 {
		p.MinKeptUserTurns = 0
	}
	if p.MinKeptMessages < 0 {
		p.MinKeptMessages = 0
	}
	if p.MinSummarizeTokens <= 0 {
		p.MinSummarizeTokens = d.MinSummarizeTokens
	}
	if p.SummaryMaxPct <= 0 {
		p.SummaryMaxPct = d.SummaryMaxPct
	}
	if p.MaxAttempts == 0 {
		p.MaxAttempts = d.MaxAttempts
	}
	if p.Temperature < 0 {
		p.Temperature = d.Temperature
	}
	if strings.TrimSpace(p.Marker) == "" {
		p.Marker = d.Marker
	}
	switch p.SummaryMode {
	case ModeTranscript, ModeIterative, ModeMapReduce:
		// legal values, kept as written (map-reduce runs the serial chain until it is implemented)
	default:
		p.SummaryMode = ModePrefixStable
	}
	if p.Mode == "" {
		p.Mode = "threshold-target"
	}
	return p
}

// Budget is the cap on the kept suffix: (threshold% − keepTarget%) × n_ctx (§2.1). The
// budget is the difference of two numbers the user understands — no extra "clever
// reservation" on top.
//
// 0 is a legal value, not a broken one: it asks for every recorded message to be replaced by the
// summary, and only the brake parameters hold that back (§2.1).
func (p Params) Budget(nctx int) int {
	if nctx <= 0 {
		return 0
	}
	d := p.ThresholdPct - p.KeepTargetPct
	if d <= 0 {
		// why clamp to 0 instead of passing the difference through: Select compares the kept suffix
		// against the budget, so a negative budget would reject even an empty tail and turn "compact
		// everything" — a legal configuration (§2.1) — back into "never compact".
		return 0
	}
	return nctx * d / 100
}

// SummaryCap is the acceptance limit for the summary itself: keepTarget% × n_ctx.
func (p Params) SummaryCap(nctx int) int {
	if nctx <= 0 {
		return 0
	}
	return nctx * p.KeepTargetPct / 100
}

// TriggerLevel is contextUsed above which compaction is considered: threshold% × n_ctx.
func (p Params) TriggerLevel(nctx int) int {
	if nctx <= 0 {
		return 0
	}
	return nctx * p.ThresholdPct / 100
}

// SummaryMaxTokens is max_tokens for the summary call: summaryMax% × n_ctx (§3.5). It
// scales with the model; a fixed value would brutally truncate long conversations.
func (p Params) SummaryMaxTokens(nctx int) int {
	if nctx <= 0 {
		return 0
	}
	return nctx * p.SummaryMaxPct / 100
}

// ShouldTrigger implements §1.3. contextUsed must be the REAL value; when it is not
// available the caller must not even check (no guessing).
func (p Params) ShouldTrigger(nctx, contextUsed int, available bool) bool {
	return p.Auto && nctx > 0 && available && contextUsed > p.TriggerLevel(nctx)
}

// Trigger identifies which of the three triggers asked for a compaction (spec §1.1).
type Trigger string

const (
	// TriggerEndOfTurn is T1: after a turn ends normally. The only automatic trigger by
	// default, because between turns the assistant message never has dangling tool calls.
	TriggerEndOfTurn Trigger = "end_of_turn"
	// TriggerInLoop is T2: before sending, inside the loop. On by default (compact.inLoop=false
	// turns it off).
	TriggerInLoop Trigger = "in_loop"
	// TriggerManual is T3: the user pressed the button. It bypasses the trigger condition and
	// the need for real usage, never the safety floors, and never the geometry of the cut (§1.1, §2.3).
	TriggerManual Trigger = "manual"
)

// Reason explains a decision, for the log line / inspector (§3.9: never fail silently).
type Reason string

const (
	ReasonOK            Reason = ""
	ReasonAutoOff       Reason = "auto compaction is off"
	ReasonNCTXUnknown   Reason = "n_ctx unknown: no basis for a budget, no compaction"
	ReasonNoUsage       Reason = "real context usage unavailable: not guessing"
	ReasonBelowTrigger  Reason = "context usage below threshold"
	ReasonTooSmall      Reason = "summarize region below minSummarizeTokens: lower it, or there is nothing worth summarising yet"
	ReasonNoBoundary    Reason = "no legal boundary keeps enough context"
	ReasonInLoopOff     Reason = "in-loop compaction is off"
	ReasonAlreadyActive Reason = "a compaction is already running"
	ReasonNotStreaming  Reason = "generation in flight: compaction never runs mid-stream"
)

// Plan describes where to cut (§2.3).
type Plan struct {
	// Cut is the index into the planning path where the kept region starts.
	Cut int
	// BoundaryID is the id of the first kept entry. nil means the boundary field is ABSENT:
	// deliberate empty kept (compact everything). It must never be confused with "id present
	// but not on the path", which is a broken boundary (§2.5).
	BoundaryID *string
	// Split marks a cut that is not a user-message span boundary: the summary covers part of a turn, or
	// the whole path (Empty). A phase-2 cut that still lands on a user message is NOT a split (§2.3).
	Split bool
	// Empty marks compact-everything (kept is empty).
	Empty bool
	// SummarizeTokens / KeptTokens are derived sizes used for the safety floors.
	SummarizeTokens int
	KeptTokens      int
	// KeptUserTurns counts user messages in the kept region.
	KeptUserTurns int
	// Budget is the value the cut had to fit.
	Budget int
	// Replaced is the number of entries the summary replaces.
	Replaced int
	// Reason is empty when the plan is usable.
	Reason Reason
}

// Select runs the two-phase, boundary-first selection of §2.3.
//
// path is the EFFECTIVE path for planning (§3.2): the same entries the assembler would
// send, i.e. [old summary(user), kept…] with the system head excluded (it is never
// summarised and never part of the search). sizes holds real per-message tokens.
//
// The kept tail is max(budget, the newest MinKeptMessages): budget is how much the user wants to
// leave room for and MinKeptMessages is how little they will accept, so when the two contradict
// each other the tail is made of the floor (§2.1).
//
// Select is pure geometry: it does not know or care which trigger asked for the cut, so an
// automatic and a manual compaction of the same path land on the same boundary (§1.1).
func Select(path []*ir.Entry, sizes *asm.Sizes, nctx int, p Params) (*Plan, bool) {
	if nctx <= 0 {
		return &Plan{Reason: ReasonNCTXUnknown}, false
	}
	budget := p.Budget(nctx) // the cap as the user set it: what the plan reports as Budget
	suffix, _ := sizes.Suffix()
	n := len(path)
	if n == 0 {
		return &Plan{Reason: ReasonTooSmall}, false
	}
	total := suffix[0]
	// why this is checked before the search and not as one more floor inside it: this is the one refusal
	// where minSummarizeTokens is certainly the blocker — the whole path is smaller than the minimum
	// worth summarising, so no cut of any kind can pass it — and reporting "no legal boundary" instead
	// sends the reader looking for a tool span when the number to change is the floor itself (§2.3).
	if total < p.MinSummarizeTokens {
		return &Plan{Reason: ReasonTooSmall}, false
	}

	// budget caps the kept tail while MinKeptMessages floors it, and the two can contradict each other:
	// threshold ≤ keepTarget sets the cap to 0 while the floor says "keep N raw messages". The floor
	// wins, and only outward — allowed is raised to the cost of the deepest legal cut that still leaves
	// N entries, never lowered, because a floor can only ever demand more (§2.1). Every entry is at
	// least one token, so suffix[] is strictly decreasing and the search below lands exactly on that
	// cut: no shallower tail fits the raised cap and no deeper one survives the floor.
	//
	// why the plan still reports the unrevised budget: "KeptTokens > Budget" is the whole story of this
	// paragraph, and it has to stay visible in the log and the inspector instead of being smoothed out.
	allowed := budget
	if p.MinKeptMessages > 0 && p.MinKeptMessages < n {
		// why not raised when MinKeptMessages >= n: keeping all of it means summarising nothing, and the
		// floors below refuse that rather than committing a compaction that replaced zero messages.
		if j := floorCut(path, p.MinKeptMessages); j >= 0 && suffix[j] > allowed {
			allowed = suffix[j]
		}
	}

	floors := func(j int) bool {
		if total-suffix[j] < p.MinSummarizeTokens {
			return false // the region is not worth a summary call
		}
		if p.MinKeptMessages > 0 && len(path)-j < p.MinKeptMessages {
			return false // the floor: made reachable by the raised cap above, never negotiable here
		}
		return true
	}
	userTurnsOK := func(j int) bool {
		if p.MinKeptUserTurns <= 0 {
			return true
		}
		count := 0
		for _, e := range path[j:] {
			if roleOf(e) == "user" {
				count++
			}
		}
		return count >= p.MinKeptUserTurns
	}

	// phase 1 — cut on a user-message span boundary (preferred: the summary covers whole turns)
	for j := 0; j < n; j++ {
		if roleOf(path[j]) != "user" {
			continue
		}
		if suffix[j] > allowed {
			continue
		}
		if !floors(j) || !userTurnsOK(j) {
			continue
		}
		id := path[j].ID
		return &Plan{
			Cut: j, BoundaryID: &id,
			SummarizeTokens: total - suffix[j], KeptTokens: suffix[j],
			KeptUserTurns: countUsers(path[j:]),
			Budget:        budget, Replaced: j,
		}, true
	}

	// phase 2 — the boundary may also sit anywhere else that is legal: before an assistant message inside
	// a user-message span (that is what makes a cut a split one), on a user message phase 1's
	// minKeptUserTurns refused (a clean turn boundary, so not a split), or at j == n, which keeps
	// nothing (compact everything).
	// why j == n is written before roleOf(path[j]) in the Split expression: that index is one past the
	// end of the path on purpose, and Go's || short-circuits.
	for j := 0; j <= n; j++ {
		if j < n && !legalBoundary(path[j]) {
			continue
		}
		if suffix[j] > allowed {
			continue
		}
		if !floors(j) {
			continue
		}
		pl := &Plan{
			Cut: j, Split: j == n || roleOf(path[j]) != "user", Empty: j == n,
			SummarizeTokens: total - suffix[j], KeptTokens: suffix[j],
			KeptUserTurns: countUsers(path[j:]),
			Budget:        budget, Replaced: j,
		}
		if j < n {
			// why minKeptUserTurns is not checked again here: the floor is a rule about turn boundaries
			// (phase 1), and a cut that reaches this line either is not a turn boundary at all or is the
			// only one that fits the tail — refusing then would turn the floor into a way to stop
			// compaction, which §2.3 does not allow.
			id := path[j].ID
			pl.BoundaryID = &id
		}
		return pl, true
	}
	return &Plan{Reason: ReasonNoBoundary}, false
}

// floorCut is the deepest cut index that still leaves keep entries at a legal boundary, or -1 when
// the path has no legal boundary that far back. Walking from n-keep towards 0 and stopping at the
// first legal entry rounds the cut OUTWARD on purpose: the kept tail grows past the floor rather
// than falling below it, which is the only reading of "keep at least N" that survives an illegal
// boundary (a tool result cannot start the kept tail, §2.2). Select only ever asks for 1 ≤ keep <
// len(path) (§2.1), but every keep is answered rather than left to index out of range.
func floorCut(path []*ir.Entry, keep int) int {
	// why keep <= 0 answers len(path): that cut keeps nothing, and keeping nothing is legal for every
	// path — phase 2 of Select writes it as j == n without a boundary check — so it is the deepest cut
	// there is, and path[len(path)] is exactly the index that does not exist.
	if keep <= 0 {
		return len(path)
	}
	for j := len(path) - keep; j >= 0; j-- {
		if legalBoundary(path[j]) {
			return j
		}
	}
	return -1
}

func roleOf(e *ir.Entry) string {
	if e == nil {
		return ""
	}
	if e.Kind == ir.KindCompaction {
		// The active summary travels with the user role and counts as a user message.
		return "user"
	}
	if e.Role == "" {
		return string(e.Kind)
	}
	return e.Role
}

// legalBoundary is §2.2: the boundary may be preceded by nothing, a user message or an
// assistant message — never a tool result and never the system head.
func legalBoundary(e *ir.Entry) bool {
	switch roleOf(e) {
	case "user", "assistant":
		return true
	}
	return false
}

// LegalBoundary is exported for the optional manual-inspector path (§4), which must
// validate exactly the same rule.
func LegalBoundary(e *ir.Entry) bool { return legalBoundary(e) }

func countUsers(path []*ir.Entry) int {
	n := 0
	for _, e := range path {
		if roleOf(e) == "user" {
			n++
		}
	}
	return n
}

// SummaryInstruction returns the summary instruction to append (§3.6). It is named apart from
// the Params.Instruction config override because a type cannot hold a field and a method of the
// same name.
func (p Params) SummaryInstruction(split bool) string {
	if split {
		if strings.TrimSpace(p.SplitInstruction) != "" {
			return p.SplitInstruction
		}
		return defaultSplitInstruction
	}
	if strings.TrimSpace(p.Instruction) != "" {
		return p.Instruction
	}
	return defaultInstruction
}

const defaultInstruction = `Please compress the conversation above into a dense summary that lets the work continue without the original messages.

Include, concretely and with no pleasantries:
1) decisions and the reasoning behind them
2) facts, names, paths, URLs
3) code changes / file operations performed
4) important tool results
5) unfinished tasks and constraints

Keep it factual and self-contained. Write in the same language as the conversation.`

const defaultSplitInstruction = `Please compress the conversation above into a dense summary that lets the work continue without the original messages. The cut falls inside the turn in progress, so write two parts:

(a) earlier conversation: decisions and their reasoning, facts, names, paths, URLs, work already completed
(b) the turn in progress: the current task goal (keep the user's wording when it is not long), how far it got, which files were touched, important tool results, the likely next step

Be concrete, no pleasantries. Write in the same language as the conversation.`

// Prefix adds the fixed marker to a summary text when creating the entry (§3.6).
func (p Params) Prefix(body string) string {
	marker := strings.TrimSpace(p.Marker)
	if marker == "" {
		return body
	}
	return marker + "\n" + strings.TrimLeft(body, " \t\n")
}

// SummaryCall is the summary request, in a shape the caller turns into a provider request.
// Keeping the type here means the policy layer never has to know about HTTP bodies, while the
// loop keeps full control of model and sampling parameters (§5.3).
type SummaryCall struct {
	Messages    []asm.Message
	Tools       []llm.Tool
	MaxTokens   int
	Temperature float64
	// Stream is always false (§3.5: the complete summary and that call's timings in one shot).
	Stream bool
}

// BuildRequest builds the summary call (§3.1 + §3.5 + §5.2 + §5.3).
//
// lastMessages must be the messages array the session actually sent last time — the same
// value produced by the assembler for the effective path, not a hand-made copy — so the
// rendered prompt shares its whole prefix with the live payload.
//
// tools must be exactly the tool array the loop sent: the chat template renders tool
// definitions at the start of the prompt, so a missing or reordered array forks at token 0.
func (p Params) BuildRequest(nctx int, lastMessages []asm.Message, tools []llm.Tool, split bool, userInstruction string) SummaryCall {
	call := SummaryCall{Temperature: p.Temperature, MaxTokens: p.SummaryMaxTokens(nctx)}
	instr := p.SummaryInstruction(split)
	if strings.TrimSpace(userInstruction) != "" {
		instr += "\n\nAdditional instruction from the user: " + strings.TrimSpace(userInstruction)
	}
	switch p.SummaryMode {
	case ModeTranscript:
		// No cache affinity by construction; used for servers that cannot take shape A.
		call.Messages = []asm.Message{{Role: "system", Content: instr}}
	default:
		msgs := make([]asm.Message, 0, len(lastMessages)+1)
		msgs = append(msgs, lastMessages...)
		msgs = append(msgs, asm.Message{Role: "user", Content: instr})
		call.Messages = msgs
		call.Tools = tools
	}
	return call
}

// Transcript renders the flat transcript of shape B (§3.1): `[role] content`, attachments as
// placeholders, tool calls degraded to a tool name — never base64 into a summary call.
func Transcript(path []*ir.Entry) string {
	var sb strings.Builder
	for _, e := range path {
		if e == nil {
			continue
		}
		role := roleOf(e)
		switch {
		case strings.TrimSpace(e.Content) != "":
			fmt.Fprintf(&sb, "[%s] %s\n", role, e.Content)
		case len(e.ToolCalls) > 0:
			names := make([]string, 0, len(e.ToolCalls))
			for _, tc := range e.ToolCalls {
				names = append(names, tc.Name)
			}
			fmt.Fprintf(&sb, "[%s called: %s]\n", role, strings.Join(names, ", "))
		case len(e.Parts) > 0:
			var bits []string
			for _, pt := range e.Parts {
				switch pt.Type {
				case ir.PartImage:
					name := pt.Name
					if name == "" {
						name = pt.MimeType
					}
					bits = append(bits, "[image: "+name+"]")
				default:
					if strings.TrimSpace(pt.Text) != "" {
						bits = append(bits, pt.Text)
					}
				}
			}
			fmt.Fprintf(&sb, "[%s] %s\n", role, strings.Join(bits, " "))
		}
	}
	return sb.String()
}

// SummarySize is the size used for the acceptance check (§3.4): the completion tokens the
// server reported, else round(UTF-8 bytes / 4) — note /4, never raw bytes.
func SummarySize(reported int, text string) int {
	if reported > 0 {
		return reported
	}
	return (len([]byte(text)) + 3) / 4
}

// Accepts is the acceptance check of §3.4. The kept region is NOT part of it in threshold
// mode: the boundary already constrained it to the budget. When n_ctx is unknown there is
// no basis for a judgement, so accept.
func (p Params) Accepts(nctx, summarySize int) bool {
	limit := p.SummaryCap(nctx)
	if limit <= 0 {
		return true
	}
	return summarySize <= limit
}

// MetaOf builds the compaction entry metadata (§3.8 step 1).
func MetaOf(p Params, plan *Plan, model string, contextUsedBefore, summarySize int) ir.CompactionMeta {
	return ir.CompactionMeta{
		BoundaryID:  plan.BoundaryID,
		Replaced:    plan.Replaced,
		IsSplitTurn: plan.Split,
		Model:       model,
		UsageBefore: contextUsedBefore,
		SummarySize: summarySize,
		SummaryMode: p.SummaryMode,
	}
}

// JSON is a small helper for debug output.
func JSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}
