package notify

import "encoding/json"

// Alert is the type of an alert, which decides the destinations it reaches.
// As a Config field it holds the set of types a destination gets, and zero
// means no choice was made.
type Alert uint8

const (
	ServerProblems Alert = 1 << iota
	FailedWork
	Results
	NewerImages
)

// AlertTypes lists every alert type in display order.
var AlertTypes = []Alert{ServerProblems, FailedWork, Results, NewerImages}

// DefaultAlerts is what a destination gets until a choice is saved. Newer
// images stay in the panel: popular images publish often.
const DefaultAlerts = ServerProblems | FailedWork | Results

const allAlerts = ServerProblems | FailedWork | Results | NewerImages

var alertNames = map[Alert]string{ServerProblems: "problems", FailedWork: "failures", Results: "results", NewerImages: "images"}

// ParseAlert returns the single alert type with this form and JSON name.
func ParseAlert(name string) (Alert, bool) {
	for a, n := range alertNames {
		if n == name {
			return a, true
		}
	}
	return 0, false
}

func (a Alert) Name() string { return alertNames[a] }

func (a Alert) Label() string {
	return map[Alert]string{ServerProblems: "Server problems", FailedWork: "Failed backups and schedules",
		Results: "Restores and updates", NewerImages: "Newer images available"}[a]
}

// Includes reports whether the set a holds type t; an empty set is the default.
func (a Alert) Includes(t Alert) bool {
	if a == 0 {
		a = DefaultAlerts
	}
	return a&t != 0
}

func (a Alert) MarshalJSON() ([]byte, error) {
	names := []string{}
	for _, t := range AlertTypes {
		if a&t != 0 {
			names = append(names, t.Name())
		}
	}
	return json.Marshal(names)
}

// UnmarshalJSON ignores names it does not know.
func (a *Alert) UnmarshalJSON(data []byte) error {
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return err
	}
	*a = 0
	for _, name := range names {
		if t, ok := ParseAlert(name); ok {
			*a |= t
		}
	}
	return nil
}
