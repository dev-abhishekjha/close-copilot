package checks

type Row struct {
	Account string
	Amount  float64 // want `float type float64 in internal/checks`
	Paise   int64
}

func sum(rs []Row) (total int64) {
	for _, r := range rs {
		total += r.Paise
	}
	return total
}
