package notify

import "context"

// Health is how a destination's latest deliveries went.
type Health struct {
	// Failing: alerts aren't reaching the destination.
	Failing bool
	// Failed: an attempt has failed since the last delivery. Since is when
	// those failures began and Reason says why in plain words.
	Failed bool
	Since  int64
	Reason string
	// Delivered: something has reached the destination. Cleanup keeps each
	// destination's newest delivered row, so this outlasts retention.
	Delivered bool
}

// failingAttempts is how many times one alert must fail in a row, about
// three minutes with the retry backoff, before its destination is failing.
// A failed test counts at once: it is a person asking whether it works.
const failingAttempts = 3

// failure is an undelivered row whose last attempt failed after the
// destination's last delivery.
type failure struct {
	kind      string
	attempts  int
	createdAt int64
	code      string
}

// health decides a destination's health from its last delivery and the
// failures since, newest attempt first.
func health(d Destination, lastDelivered int64, failures []failure) Health {
	h := Health{Delivered: lastDelivered > 0, Failed: len(failures) > 0}
	if !h.Failed {
		return h
	}
	h.Since = failures[0].createdAt
	for _, f := range failures {
		h.Failing = h.Failing || f.kind == "test" || f.attempts >= failingAttempts
		h.Since = min(h.Since, f.createdAt)
	}
	h.Since = max(h.Since, lastDelivered)
	h.Reason = "Failure details are unavailable."
	if message, ok := failureMessage(d, failures[0].code); ok {
		h.Reason = message
	}
	return h
}

// Health reports every destination's health; a destination with no
// deliveries is absent.
func (s *Service) Health(ctx context.Context) (map[Destination]Health, error) {
	delivered := map[Destination]int64{}
	rows, err := s.DB.QueryContext(ctx, "SELECT destination, MAX(delivered_at) FROM notification_deliveries WHERE state='delivered' GROUP BY destination")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d string
		var at int64
		if err := rows.Scan(&d, &at); err != nil {
			rows.Close()
			return nil, err
		}
		delivered[Destination(d)] = at
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	failures := map[Destination][]failure{}
	rows, err = s.DB.QueryContext(ctx, `SELECT d.destination, d.kind, d.attempts, d.created_at, d.failure_code
    FROM notification_deliveries d WHERE d.state!='delivered' AND d.failure_code IS NOT NULL AND d.last_attempt_at >=
      COALESCE((SELECT MAX(x.delivered_at) FROM notification_deliveries x WHERE x.destination=d.destination AND x.state='delivered'), -1)
    ORDER BY d.last_attempt_at DESC, d.rowid DESC`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d string
		var f failure
		if err := rows.Scan(&d, &f.kind, &f.attempts, &f.createdAt, &f.code); err != nil {
			rows.Close()
			return nil, err
		}
		failures[Destination(d)] = append(failures[Destination(d)], f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := map[Destination]Health{}
	for _, d := range Destinations {
		if _, ok := delivered[d]; ok || len(failures[d]) > 0 {
			result[d] = health(d, delivered[d], failures[d])
		}
	}
	return result, nil
}

// Problem is a destination that is on and failing.
type Problem struct {
	Destination Destination
	Health
}

// Problems lists the destinations that are on and failing, in display order.
func (s *Service) Problems(ctx context.Context) ([]Problem, error) {
	all := s.settings(ctx)
	healths, err := s.Health(ctx)
	if err != nil {
		return nil, err
	}
	var problems []Problem
	for _, d := range Destinations {
		if d.ready(all[d]) && healths[d].Failing {
			problems = append(problems, Problem{d, healths[d]})
		}
	}
	return problems, nil
}
