package handler

import (
	"testing"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
)

func TestCorpusResponseAbstainsWhenTheCollectionDoesNotAnswer(t *testing.T) {
	res := &retrieval.CorpusResult{
		Evidence:   []retrieval.CorpusEvidence{{DocumentID: "d", Page: 1, Text: "unrelated", Confidence: 0.2}},
		Documents:  []retrieval.CorpusDocScore{},
		Hops:       []retrieval.CorpusHop{},
		Coverage:   0.02,
		Answerable: false,
	}
	resp := corpusResponse("q", res, time.Now(), true)
	if resp["abstained"] != true || len(resp["evidence"].([]retrieval.CorpusEvidence)) != 0 || resp["abstain_reason"] == nil {
		t.Fatalf("an unanswerable collection should abstain with no evidence: %+v", resp)
	}
	kept := corpusResponse("q", res, time.Now(), false)
	if kept["abstained"] != false || len(kept["evidence"].([]retrieval.CorpusEvidence)) != 1 {
		t.Fatalf("abstain: false keeps the evidence: %+v", kept)
	}
	res.Answerable, res.Coverage = true, 0.4
	if a := corpusResponse("q", res, time.Now(), true); a["abstained"] != false {
		t.Fatalf("an answerable collection never abstains: %+v", a)
	}
}

func TestAbstainDefaultsToTrue(t *testing.T) {
	no := false
	if !abstains(corpusRequest{}) || abstains(corpusRequest{Abstain: &no}) {
		t.Fatal("abstain defaults to true and can be turned off")
	}
}
