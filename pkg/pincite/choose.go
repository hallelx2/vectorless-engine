package pincite

import (
	"context"
	"fmt"
	"strings"

	"github.com/hallelx2/llmgate"
)

// shortlistSize is how many sentences the Judge chooses among per
// claim. Shortlist keeps it to the plausible ones.
const shortlistSize = 10

// noneOption is the Judge's way to say no sentence supports the claim.
// Without it the distribution is forced across options that are all
// wrong, and a highlight would land on the least-bad sentence.
const noneOption = "none"

// minChoiceProbability: below this the Judge's pick is not trusted and
// the citation degrades to page level rather than highlight a guess.
const minChoiceProbability = 0.35

// Claim asks which sentence of a page supports one claim.
type Claim struct {
	Claim      string
	Candidates []string
}

// Choice is the sentence chosen for a claim: an index into its
// Candidates, or -1 when nothing supports it. P is the Judge's
// probability for the pick (1 when chosen lexically without a Judge).
type Choice struct {
	Index int
	P     float64
	By    string // "judge" | "lexical" | ""
}

// Choose picks, for every claim, the one sentence of its page that
// states it. This is a closed-set decision, so it goes to the Judge in
// one batched request — select, don't generate. A model never writes
// the quote: the quote is the page's own sentence, which is why it can
// always be located on the page afterwards.
//
// With no Judge, or when the Judge fails, each claim falls back to the
// sentence with the strongest lexical overlap. The fallback is
// recorded in Choice.By so a caller can tell which kind of pick it is.
func Choose(ctx context.Context, judge llmgate.Judge, claims []Claim) ([]Choice, llmgate.Usage, error) {
	out := make([]Choice, len(claims))
	shortlists := make([][]int, len(claims))
	for i, c := range claims {
		out[i] = Choice{Index: -1}
		shortlists[i] = Shortlist(c.Claim, c.Candidates, shortlistSize)
	}
	lexical := func(i int) Choice {
		best := bestLexical(claims[i].Claim, claims[i].Candidates)
		if best < 0 {
			return Choice{Index: -1}
		}
		return Choice{Index: best, P: 1, By: "lexical"}
	}

	var usage llmgate.Usage
	state := map[string]any{}
	questions := map[string]llmgate.Question{}
	for i, c := range claims {
		sl := shortlists[i]
		if len(sl) == 0 {
			continue
		}
		if len(sl) == 1 && judge == nil {
			out[i] = Choice{Index: sl[0], P: 1, By: "lexical"}
			continue
		}
		key := fmt.Sprintf("c_%d", i)
		sentences := map[string]string{}
		opts := make(llmgate.ChoiceOptions, 0, len(sl)+1)
		for _, idx := range sl {
			name := fmt.Sprintf("s%d", idx)
			sentences[name] = c.Candidates[idx]
			opts = append(opts, llmgate.ChoiceOption{Name: name})
		}
		opts = append(opts, llmgate.ChoiceOption{Name: noneOption, Description: "No listed sentence states this claim"})
		state[key] = map[string]any{"claim": c.Claim, "sentences": sentences}
		questions[key] = llmgate.Choice{
			Instructions: fmt.Sprintf(
				"`%s.claim` is one claim from an answer about a document. `%s.sentences` are sentences "+
					"from the page the claim cites, keyed by name. Which sentence states the fact the claim "+
					"relies on — the figure, the date, the statement itself? Pick `none` if no sentence states it; "+
					"a sentence that only mentions the topic does not.", key, key),
			Options: opts,
		}
	}
	if len(questions) == 0 {
		return out, usage, nil
	}
	if judge == nil {
		for i := range claims {
			if out[i].Index < 0 {
				out[i] = lexical(i)
			}
		}
		return out, usage, nil
	}

	res, err := judge.Judge(ctx, llmgate.JudgeRequest{State: state, Questions: questions})
	if err != nil {
		for i := range claims {
			if out[i].Index < 0 {
				out[i] = lexical(i)
			}
		}
		return out, usage, err
	}
	usage = res.Usage
	for key := range questions {
		var i int
		fmt.Sscanf(key, "c_%d", &i)
		ans, aerr := res.Choice(key)
		if aerr != nil {
			out[i] = lexical(i)
			continue
		}
		if ans.Choice == noneOption || !strings.HasPrefix(ans.Choice, "s") {
			out[i] = Choice{Index: -1, P: ans.Probabilities[noneOption], By: "judge"}
			continue
		}
		p := ans.Probabilities[ans.Choice]
		if p < minChoiceProbability {
			out[i] = Choice{Index: -1, P: p, By: "judge"}
			continue
		}
		var idx int
		if _, serr := fmt.Sscanf(ans.Choice, "s%d", &idx); serr != nil || idx < 0 || idx >= len(claims[i].Candidates) {
			out[i] = lexical(i)
			continue
		}
		out[i] = Choice{Index: idx, P: p, By: "judge"}
	}
	return out, usage, nil
}

// bestLexical picks the candidate sharing the most of the claim's
// content tokens, numbers weighted as in Shortlist.
func bestLexical(claim string, candidates []string) int {
	ranked := Shortlist(claim, candidates, len(candidates))
	best, bestScore := -1, 0.0
	ct := map[string]bool{}
	for _, t := range Tokenize(claim) {
		if !stopword[t] {
			ct[t] = true
		}
	}
	for _, i := range ranked {
		seen := map[string]bool{}
		s := 0.0
		toks := Tokenize(candidates[i])
		for _, t := range toks {
			if ct[t] && !seen[t] {
				seen[t] = true
				if isNumeric(t) {
					s += 3
				} else {
					s++
				}
			}
		}
		// Prefer the tighter sentence on a tie: it claims less.
		if s > bestScore || (s == bestScore && best >= 0 && len(toks) < len(Tokenize(candidates[best]))) {
			best, bestScore = i, s
		}
	}
	return best
}
