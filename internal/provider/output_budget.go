package provider

// OutputBudgetProvider reports the output budget used when a request leaves it unset.
type OutputBudgetProvider interface {
	OutputBudget() int
}

// SharedWindowOutputProvider reports whether input and output share one window.
type SharedWindowOutputProvider interface {
	SharesContextWindow() bool
}
