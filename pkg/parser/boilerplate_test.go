package parser

import "testing"

func TestIsBoilerplateLine(t *testing.T) {
	boiler := []string{
		"Provided proper attribution is provided, Google hereby grants permission to",
		"reproduce the tables and figures in this paper solely for use in journalistic or",
		"All Rights Reserved.",
		"This work is licensed under a Creative Commons Attribution 4.0 License.",
		"arXiv:1706.03762v7 [cs.CL] 2 Aug 2023",
		"Preprint. Under review.",
		"scholarly works.", // short license-tail fragment
	}
	for _, s := range boiler {
		if !isBoilerplateLine(s) {
			t.Errorf("expected boilerplate: %q", s)
		}
	}
	content := []string{
		"Attention Is All You Need",
		"3 Model Architecture",
		"The dominant sequence transduction models are based on attention.",
		"We grant the model permission to attend to all positions.", // 'permission' but not boilerplate
		"Results",
		"References",
		"Recommendation: initiate low-dose therapy (evidence grade A).",
		"This study was published in several scholarly works over the past decade.", // 'scholarly works' inside real prose
	}
	for _, s := range content {
		if isBoilerplateLine(s) {
			t.Errorf("false positive on content: %q", s)
		}
	}
}
