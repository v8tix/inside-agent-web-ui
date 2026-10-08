// Package pacer spaces out the updates of a text that grows: the first goes at once, then at most
// one for each interval, always with the newest text. The interval follows the cost of the last send.
package pacer

import "time"

// Pacer belongs to one goroutine. Base is the shortest interval. Factor times the cost of
// the last send is the interval when that is longer, so sending stays a fixed share of the work.
type Pacer struct {
	Base    time.Duration
	Factor  int
	held    string
	waiting bool
	last    time.Time
	cost    time.Duration
}

// Offer takes the newest text and returns it if it is time to send it.
func (p *Pacer) Offer(now time.Time, text string) (string, bool) {
	p.held, p.waiting = text, true
	return p.Due(now)
}

// Due returns the waiting text once the interval has passed. Call it also when a timer fires.
func (p *Pacer) Due(now time.Time) (string, bool) {
	interval := max(p.Base, time.Duration(p.Factor)*p.cost)
	if !p.waiting || now.Sub(p.last) < interval {
		return "", false
	}
	p.waiting = false
	return p.held, true
}

// Sent records when a send ended and what it cost. Call it after every send.
func (p *Pacer) Sent(now time.Time, cost time.Duration) { p.last, p.cost = now, cost }
