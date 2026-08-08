package cli

import "reasonix/internal/control"

func (m *chatTUI) noticeDeprecatedGoalBudget(cmd control.GoalCommand) {
	if cmd.DeprecatedBudgetFlag {
		m.notice(control.GoalBudgetFlagDeprecatedNotice)
	}
}
