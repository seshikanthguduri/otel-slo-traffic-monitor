package slo

import "math"

// ErrorBudget is the allowed error ratio for an objective (1 - objective).
func ErrorBudget(objective float64) float64 { return 1 - objective }

// BurnRate is how fast the budget is being consumed relative to "exactly on
// budget". 1.0 exhausts the budget precisely at the end of the window.
func BurnRate(errorRatio, objective float64) float64 {
	b := ErrorBudget(objective)
	if b <= 0 {
		return math.Inf(1)
	}
	return errorRatio / b
}

// BudgetRemaining returns the fraction of error budget left over the SLO
// window. It is negative once the budget is overspent.
func BudgetRemaining(errorRatio, objective float64) float64 {
	return 1 - BurnRate(errorRatio, objective)
}

// SLAState is the contractual status.
type SLAState string

const (
	SLAMet      SLAState = "MET"
	SLAAtRisk   SLAState = "AT_RISK"
	SLABreached SLAState = "BREACHED"
	SLANoData   SLAState = "NO_DATA"
)

// SLAStatus is the SLA evaluation result.
type SLAStatus struct {
	Target          float64  `json:"target"`
	State           SLAState `json:"state"`
	BudgetRemaining float64  `json:"budget_remaining"`
	CreditPercent   float64  `json:"credit_percent"`
}

// EvaluateSLA computes compliance and service credits for an SLI value.
// AT_RISK means the SLA still holds but the internal SLO is already breached
// or less than 25% of the contractual budget is left.
func EvaluateSLA(sla SLA, sli, objective float64, hasData bool) SLAStatus {
	st := SLAStatus{Target: sla.Target}
	if !hasData {
		st.State = SLANoData
		return st
	}
	st.BudgetRemaining = BudgetRemaining(1-sli, sla.Target)
	switch {
	case sli < sla.Target:
		st.State = SLABreached
	case sli < objective || st.BudgetRemaining < 0.25:
		st.State = SLAAtRisk
	default:
		st.State = SLAMet
	}
	for _, t := range sla.Credits {
		if sli < t.Below && t.Percent > st.CreditPercent {
			st.CreditPercent = t.Percent
		}
	}
	return st
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }
