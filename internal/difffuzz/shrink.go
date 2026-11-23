package difffuzz

// Shrink makes s smaller while check keeps reporting a divergence of the
// same class as the original: chunks of steps first (halving the chunk),
// then single ops inside transactions. The final step stays. budget bounds
// the candidate runs; check may return nil for a candidate that passes.
func Shrink(s Scenario, class string, budget int, check func(Scenario) *Divergence) Scenario {
	runs := 0
	fails := func(c Scenario) bool {
		if runs >= budget {
			return false
		}
		runs++
		d := check(c)
		return d != nil && d.Class() == class
	}
	best := s
	// Steps, without the final one.
	for chunk := len(best.Steps) / 2; chunk >= 1; chunk /= 2 {
		for i := 0; i+chunk <= len(best.Steps)-1 && runs < budget; {
			c := best
			c.Steps = append(append([]Step(nil), best.Steps[:i]...), best.Steps[i+chunk:]...)
			if fails(c) {
				best = c
				continue
			}
			i += chunk
		}
	}
	// Ops inside transactions.
	for i := 0; i < len(best.Steps) && runs < budget; i++ {
		st := best.Steps[i]
		if st.K != StepTxn {
			continue
		}
		for k := 0; k < len(best.Steps[i].Ops) && len(best.Steps[i].Ops) > 1 && runs < budget; {
			c := best
			c.Steps = append([]Step(nil), best.Steps...)
			ops := best.Steps[i].Ops
			c.Steps[i].Ops = append(append([]Op(nil), ops[:k]...), ops[k+1:]...)
			if fails(c) {
				best = c
				continue
			}
			k++
		}
	}
	return best
}
