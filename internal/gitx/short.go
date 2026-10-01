package gitx

// shortLen is how many leading characters of a commit SHA graft prints.
const shortLen = 12

// Short is the printed form of a commit SHA; a value shorter than shortLen,
// read from a file on the target or from the environment, comes back whole.
func Short(sha string) string {
	return sha[:min(shortLen, len(sha))]
}
