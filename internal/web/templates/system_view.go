package templates

// SystemRow is one service card on the System page. Status is ok, down or
// unknown; Text is fixed wording, never an error message.
type SystemRow struct {
	ID, Title, Status, Text string
	Facts                   []Fact
}

// SystemBudget is the daily model budget card. Tone is ok, warn or danger;
// Spent and Of are already formatted dollar amounts (Of is empty when no
// budget is set).
type SystemBudget struct {
	Status, Tone, Text string
	Spent, Of, Percent string
}

// SystemPage is the System page. Checked is the time of the last check,
// formatted, or empty before any check.
type SystemPage struct {
	User, Role, CSRFToken string
	NotConfigured         bool
	Rows                  []SystemRow
	Budget                SystemBudget
	Checked               string
}

// systemChipFor maps a status word to the chip class, icon and label.
func systemChipFor(status string) chip {
	switch status {
	case "ok":
		return chip{"ok", "check-circle", "OK"}
	case "down":
		return chip{"danger", "alert", "Down"}
	}
	return chip{"neutral", "clock", "Not checked"}
}

// healthText is the screen-reader text of the top bar's health dot.
func healthText(h string) string {
	switch h {
	case "ok":
		return "System: all checks passing"
	case "down":
		return "System: a check is failing"
	}
	return "System: not checked yet"
}

func healthClass(h string) string {
	switch h {
	case "ok", "down":
		return "health health-" + h
	}
	return "health health-unknown"
}

func healthIcon(h string) string {
	switch h {
	case "ok":
		return "check-circle"
	case "down":
		return "alert"
	}
	return "clock"
}

// budgetHint is the line under the spend: "of $5.00 (25%)" or empty.
func budgetHint(b SystemBudget) string {
	if b.Of == "" {
		return ""
	}
	return "of " + b.Of + " (" + b.Percent + ")"
}
