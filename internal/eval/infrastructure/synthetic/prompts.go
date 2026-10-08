package synthetic

// What the probes say to the scripted agent. Each carries the id of a reply in
// data/synthetic_replies.json, which is how the agent knows what to do: a long
// reply that takes time to write, and one that fails the first time it is asked.
const (
	LongProbePrompt  = "Write a long essay about the printing press. [probe-long]"
	ShortProbePrompt = "Reply with the single word OK."
	FlakyProbePrompt = "Are you back? [probe-flaky]"
)
