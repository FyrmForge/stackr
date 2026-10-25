package admin

// Seams for the external tests.
var (
	KnobForms = knobForms
	OwnEditor = ownEditor
	Badges    = badges
	// ActionWord is the security log's word for an action key.
	ActionWord = func(action string) string {
		label := map[string]string{}
		for _, a := range securityActions {
			label[a.Key] = a.Label
		}
		return actionWord(label, action)
	}
)
