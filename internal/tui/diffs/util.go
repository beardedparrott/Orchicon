package diffs

// itoa is a small strconv-free integer-to-string for the diff renderer
// (line numbers). Avoids importing strconv in hot render paths; correctness
// over micro-perf — the pane renders at human speed.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	digits := make([]byte, 0, 12)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}
