package retrieval

// The names a customer sees. The Judge is an internal model choice and
// never appears in a response, the SDKs or the docs, so the strategy built
// on it is reported as "navigate" and accepted under that name.
const publicNameJudgeWalk = "navigate"

// PublicName is the strategy name to put in a customer-facing response.
func PublicName(name string) string {
	if name == strategyNameJudgeWalk {
		return publicNameJudgeWalk
	}
	return name
}

// InternalName resolves a name a customer sent back (a per-request
// strategy override) to the configured strategy it stands for.
func InternalName(name string) string {
	if name == publicNameJudgeWalk {
		return strategyNameJudgeWalk
	}
	return name
}
