package checks

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/cloudflare/pint/internal/discovery"
	"github.com/cloudflare/pint/internal/output"
	"github.com/cloudflare/pint/internal/parser"
	"github.com/cloudflare/pint/internal/promapi"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	promParser "github.com/prometheus/prometheus/promql/parser"
)

const (
	SeriesCheckName = "promql/series"
)

func NewSeriesCheck(prom *promapi.FailoverGroup) SeriesCheck {
	return SeriesCheck{prom: prom}
}

type SeriesCheck struct {
	prom *promapi.FailoverGroup
}

func (c SeriesCheck) String() string {
	return fmt.Sprintf("%s(%s)", SeriesCheckName, c.prom.Name())
}

func (c SeriesCheck) Reporter() string {
	return SeriesCheckName
}

func (c SeriesCheck) Check(ctx context.Context, rule parser.Rule, entries []discovery.Entry) (problems []Problem) {
	expr := rule.Expr()

	if expr.SyntaxError != nil {
		return
	}

	rangeLookback := time.Hour * 24 * 7
	rangeStep := time.Minute * 5

	done := map[string]bool{}
	for _, selector := range getSelectors(expr.Query) {
		if _, ok := done[selector.String()]; ok {
			continue
		}

		done[selector.String()] = true

		bareSelector := stripLabels(selector)
		c1 := fmt.Sprintf("disable %s(%s)", SeriesCheckName, selector.String())
		c2 := fmt.Sprintf("disable %s(%s)", SeriesCheckName, bareSelector.String())
		if rule.HasComment(c1) || rule.HasComment(c2) {
			done[selector.String()] = true
			continue
		}

		metricName := selector.Name
		if metricName == "" {
			for _, lm := range selector.LabelMatchers {
				if lm.Name == labels.MetricName && lm.Type == labels.MatchEqual {
					metricName = lm.Value
					break
				}
			}
		}

		// 0. Special case for alert metrics
		if metricName == "ALERTS" || metricName == "ALERTS_FOR_STATE" {
			var alertname string
			for _, lm := range selector.LabelMatchers {
				if lm.Name == "alertname" && lm.Type != labels.MatchRegexp && lm.Type != labels.MatchNotRegexp {
					alertname = lm.Value
				}
			}
			var arEntry *discovery.Entry
			if alertname != "" {
				for _, entry := range entries {
					entry := entry
					if entry.Rule.AlertingRule != nil &&
						entry.Rule.Error.Err == nil &&
						entry.Rule.AlertingRule.Alert.Value.Value == alertname {
						arEntry = &entry
						break
					}
				}
				if arEntry != nil {
					log.Debug().Stringer("selector", &selector).Str("path", arEntry.Path).Msg("Metric is provided by alerting rule")
					problems = append(problems, Problem{
						Fragment: selector.String(),
						Lines:    expr.Lines(),
						Reporter: c.Reporter(),
						Text:     fmt.Sprintf("%s metric is generated by alerts and found alerting rule named %q", selector.String(), alertname),
						Severity: Information,
					})
				} else {
					problems = append(problems, Problem{
						Fragment: selector.String(),
						Lines:    expr.Lines(),
						Reporter: c.Reporter(),
						Text:     fmt.Sprintf("%s metric is generated by alerts but didn't found any rule named %q", selector.String(), alertname),
						Severity: Bug,
					})
				}
			}
			// ALERTS{} query with no alertname, all good
			continue
		}

		labelNames := []string{}
		for _, lm := range selector.LabelMatchers {
			if lm.Name != labels.MetricName {
				labelNames = append(labelNames, lm.Name)
			}
		}

		// 1. If foo{bar, baz} is there -> GOOD
		log.Debug().Str("check", c.Reporter()).Stringer("selector", &selector).Msg("Checking if selector returns anything")
		count, _, err := c.instantSeriesCount(ctx, fmt.Sprintf("count(%s)", selector.String()))
		if err != nil {
			problems = append(problems, c.queryProblem(err, selector.String(), expr))
			continue
		}
		if count > 0 {
			log.Debug().Str("check", c.Reporter()).Stringer("selector", &selector).Msg("Found series, skipping further checks")
			continue
		}

		// 2. If foo was NEVER there -> BUG
		log.Debug().Str("check", c.Reporter()).Stringer("selector", &bareSelector).Msg("Checking if base metric has historical series")
		trs, err := c.serieTimeRanges(ctx, fmt.Sprintf("count(%s)", bareSelector.String()), rangeLookback, rangeStep)
		if err != nil {
			problems = append(problems, c.queryProblem(err, bareSelector.String(), expr))
			continue
		}
		if len(trs.ranges) == 0 {
			// Check if we have recording rule that provides this metric before we give up
			var rrEntry *discovery.Entry
			for _, entry := range entries {
				entry := entry
				if entry.Rule.RecordingRule != nil &&
					entry.Rule.Error.Err == nil &&
					entry.Rule.RecordingRule.Record.Value.Value == bareSelector.String() {
					rrEntry = &entry
					break
				}
			}
			if rrEntry != nil {
				// Validate recording rule instead
				log.Debug().Stringer("selector", &bareSelector).Str("path", rrEntry.Path).Msg("Metric is provided by recording rule")
				problems = append(problems, Problem{
					Fragment: bareSelector.String(),
					Lines:    expr.Lines(),
					Reporter: c.Reporter(),
					Text: fmt.Sprintf("%s didn't have any series for %q metric in the last %s but found recording rule that generates it, skipping further checks",
						promText(c.prom.Name(), trs.uri), bareSelector.String(), trs.sinceDesc(trs.from)),
					Severity: Information,
				})
				continue
			}

			problems = append(problems, Problem{
				Fragment: bareSelector.String(),
				Lines:    expr.Lines(),
				Reporter: c.Reporter(),
				Text: fmt.Sprintf("%s didn't have any series for %q metric in the last %s",
					promText(c.prom.Name(), trs.uri), bareSelector.String(), trs.sinceDesc(trs.from)),
				Severity: Bug,
			})
			log.Debug().Str("check", c.Reporter()).Stringer("selector", &bareSelector).Msg("No historical series for base metric")
			continue
		}

		highChurnLabels := []string{}

		// 3. If foo is ALWAYS/SOMETIMES there BUT {bar OR baz} is NEVER there -> BUG
		for _, name := range labelNames {
			l := stripLabels(selector)
			l.LabelMatchers = append(l.LabelMatchers, labels.MustNewMatcher(labels.MatchRegexp, name, ".+"))
			log.Debug().Str("check", c.Reporter()).Stringer("selector", &l).Str("label", name).Msg("Checking if base metric has historical series with required label")
			trsLabelCount, err := c.serieTimeRanges(ctx, fmt.Sprintf("count(%s) by (%s)", l.String(), name), rangeLookback, rangeStep)
			if err != nil {
				problems = append(problems, c.queryProblem(err, selector.String(), expr))
				continue
			}

			labelRanges := trsLabelCount.withLabelName(name)
			if len(labelRanges) == 0 {
				problems = append(problems, Problem{
					Fragment: selector.String(),
					Lines:    expr.Lines(),
					Reporter: c.Reporter(),
					Text: fmt.Sprintf(
						"%s has %q metric but there are no series with %q label in the last %s",
						promText(c.prom.Name(), trsLabelCount.uri), bareSelector.String(), name, trsLabelCount.sinceDesc(trsLabelCount.from)),
					Severity: Bug,
				})
				log.Debug().Str("check", c.Reporter()).Stringer("selector", &l).Str("label", name).Msg("No historical series with label used for the query")
			}

			if len(trsLabelCount.labelValues(name)) == len(trsLabelCount.ranges) && trsLabelCount.avgLife() < (trsLabelCount.duration()/2) {
				highChurnLabels = append(highChurnLabels, name)
			}
		}
		if len(problems) > 0 {
			continue
		}

		// 4. If foo was ALWAYS there but it's NO LONGER there (for more than min-age) -> BUG
		if len(trs.ranges) == 1 &&
			!trs.oldest().After(trs.from.Add(rangeStep)) &&
			trs.newest().Before(trs.until.Add(rangeStep*-1)) {

			minAge, p := c.getMinAge(rule, selector)
			if len(p) > 0 {
				problems = append(problems, p...)
			}

			if !trs.newest().Before(trs.until.Add(time.Duration(minAge) * -1)) {
				log.Debug().
					Str("check", c.Reporter()).
					Stringer("selector", &selector).
					Str("min-age", output.HumanizeDuration(minAge)).
					Str("last-seen", trs.sinceDesc(trs.newest())).
					Msg("Series disappeared from prometheus but for less then configured min-age")
				continue
			}

			problems = append(problems, Problem{
				Fragment: bareSelector.String(),
				Lines:    expr.Lines(),
				Reporter: c.Reporter(),
				Text: fmt.Sprintf(
					"%s doesn't currently have %q, it was last present %s ago",
					promText(c.prom.Name(), trs.uri), bareSelector.String(), trs.sinceDesc(trs.newest())),
				Severity: Bug,
			})
			log.Debug().Str("check", c.Reporter()).Stringer("selector", &bareSelector).Msg("Series disappeared from prometheus")
			continue
		}

		for _, lm := range selector.LabelMatchers {
			if lm.Name == labels.MetricName {
				continue
			}
			if lm.Type != labels.MatchEqual && lm.Type != labels.MatchRegexp {
				continue
			}
			if c.isLabelValueIgnored(rule, selector, lm.Name) {
				log.Debug().Stringer("selector", &selector).Str("label", lm.Name).Msg("Label check disabled by comment")
				continue
			}
			labelSelector := promParser.VectorSelector{
				Name:          metricName,
				LabelMatchers: []*labels.Matcher{lm},
			}
			log.Debug().Str("check", c.Reporter()).Stringer("selector", &labelSelector).Stringer("matcher", lm).Msg("Checking if there are historical series matching filter")

			trsLabel, err := c.serieTimeRanges(ctx, fmt.Sprintf("count(%s)", labelSelector.String()), rangeLookback, rangeStep)
			if err != nil {
				problems = append(problems, c.queryProblem(err, labelSelector.String(), expr))
				continue
			}

			// 5. If foo is ALWAYS/SOMETIMES there BUT {bar OR baz} value is NEVER there -> BUG
			if len(trsLabel.ranges) == 0 {
				text := fmt.Sprintf(
					"%s has %q metric with %q label but there are no series matching {%s} in the last %s",
					promText(c.prom.Name(), trsLabel.uri), bareSelector.String(), lm.Name, lm.String(), trsLabel.sinceDesc(trs.from))
				s := Bug
				for _, name := range highChurnLabels {
					if lm.Name == name {
						s = Warning
						text += fmt.Sprintf(", %q looks like a high churn label", name)
						break
					}
				}

				problems = append(problems, Problem{
					Fragment: selector.String(),
					Lines:    expr.Lines(),
					Reporter: c.Reporter(),
					Text:     text,
					Severity: s,
				})
				log.Debug().Str("check", c.Reporter()).Stringer("selector", &selector).Stringer("matcher", lm).Msg("No historical series matching filter used in the query")
				continue
			}

			// 6. If foo is ALWAYS/SOMETIMES there AND {bar OR baz} used to be there ALWAYS BUT it's NO LONGER there -> BUG
			if len(trsLabel.ranges) == 1 &&
				!trsLabel.oldest().After(trsLabel.until.Add(rangeLookback-1).Add(rangeStep)) &&
				trsLabel.newest().Before(trsLabel.until.Add(rangeStep*-1)) {

				minAge, p := c.getMinAge(rule, selector)
				if len(p) > 0 {
					problems = append(problems, p...)
				}

				if !trsLabel.newest().Before(trsLabel.until.Add(time.Duration(minAge) * -1)) {
					log.Debug().
						Str("check", c.Reporter()).
						Stringer("selector", &selector).
						Str("min-age", output.HumanizeDuration(minAge)).
						Str("last-seen", trs.sinceDesc(trsLabel.newest())).
						Msg("Series disappeared from prometheus but for less then configured min-age")
					continue
				}

				problems = append(problems, Problem{
					Fragment: labelSelector.String(),
					Lines:    expr.Lines(),
					Reporter: c.Reporter(),
					Text: fmt.Sprintf(
						"%s has %q metric but doesn't currently have series matching {%s}, such series was last present %s ago",
						promText(c.prom.Name(), trs.uri), bareSelector.String(), lm.String(), trsLabel.sinceDesc(trsLabel.newest())),
					Severity: Bug,
				})
				log.Debug().Str("check", c.Reporter()).Stringer("selector", &selector).Stringer("matcher", lm).Msg("Series matching filter disappeared from prometheus ")
				continue
			}

			// 7. if foo is ALWAYS/SOMETIMES there BUT {bar OR baz} value is SOMETIMES there -> WARN
			if len(trsLabel.ranges) > 1 {
				problems = append(problems, Problem{
					Fragment: selector.String(),
					Lines:    expr.Lines(),
					Reporter: c.Reporter(),
					Text: fmt.Sprintf(
						"metric %q with label {%s} is only sometimes present on %s with average life span of %s",
						bareSelector.String(), lm.String(), promText(c.prom.Name(), trs.uri),
						output.HumanizeDuration(trsLabel.avgLife())),
					Severity: Warning,
				})
				log.Debug().Str("check", c.Reporter()).Stringer("selector", &selector).Stringer("matcher", lm).Msg("Series matching filter are only sometimes present")
			}
		}
		if len(problems) > 0 {
			continue
		}

		// 8. If foo is SOMETIMES there -> WARN
		if len(trs.ranges) > 1 {
			problems = append(problems, Problem{
				Fragment: bareSelector.String(),
				Lines:    expr.Lines(),
				Reporter: c.Reporter(),
				Text: fmt.Sprintf(
					"metric %q is only sometimes present on %s with average life span of %s in the last %s",
					bareSelector.String(), promText(c.prom.Name(), trs.uri), output.HumanizeDuration(trs.avgLife()), trs.sinceDesc(trs.from)),
				Severity: Warning,
			})
			log.Debug().Str("check", c.Reporter()).Stringer("selector", &bareSelector).Msg("Metric only sometimes present")
		}
	}

	return
}

func (c SeriesCheck) queryProblem(err error, selector string, expr parser.PromQLExpr) Problem {
	text, severity := textAndSeverityFromError(err, c.Reporter(), c.prom.Name(), Bug)
	return Problem{
		Fragment: selector,
		Lines:    expr.Lines(),
		Reporter: c.Reporter(),
		Text:     text,
		Severity: severity,
	}
}

func (c SeriesCheck) instantSeriesCount(ctx context.Context, query string) (int, string, error) {
	qr, err := c.prom.Query(ctx, query)
	if err != nil {
		return 0, "", err
	}

	var series int
	for _, s := range qr.Series {
		series += int(s.Value)
	}

	return series, qr.URI, nil
}

func (c SeriesCheck) serieTimeRanges(ctx context.Context, query string, lookback, step time.Duration) (tr *timeRanges, err error) {
	qr, err := c.prom.RangeQuery(ctx, query, lookback, step)
	if err != nil {
		return nil, err
	}

	tr = &timeRanges{
		uri:   qr.URI,
		from:  qr.Start,
		until: qr.End,
		step:  step,
	}
	var ts time.Time
	for _, s := range qr.Samples {
		for _, v := range s.Values {
			ts = v.Timestamp.Time()

			var found bool
			for i := range tr.ranges {
				if tr.ranges[i].labels.Equal(model.LabelSet(s.Metric)) &&
					!ts.Before(tr.ranges[i].start) &&
					!ts.After(tr.ranges[i].end) {
					tr.ranges[i].end = ts.Add(step)
					found = true
					break
				}
			}
			if !found {
				tr.ranges = append(tr.ranges, timeRange{
					labels: model.LabelSet(s.Metric),
					start:  ts,
					end:    ts.Add(step),
				})
			}
		}
	}

	return tr, nil
}

func (c SeriesCheck) getMinAge(rule parser.Rule, selector promParser.VectorSelector) (minAge time.Duration, problems []Problem) {
	minAge = time.Hour * 2

	bareSelector := stripLabels(selector)
	for _, s := range [][]string{
		{"rule/set", c.Reporter(), "min-age"},
		{"rule/set", fmt.Sprintf("%s(%s)", c.Reporter(), bareSelector.String()), "min-age"},
		{"rule/set", fmt.Sprintf("%s(%s)", c.Reporter(), selector.String()), "min-age"},
	} {
		if cmt, ok := rule.GetComment(s...); ok {
			dur, err := model.ParseDuration(cmt.Value)
			if err != nil {
				problems = append(problems, Problem{
					Fragment: cmt.String(),
					Lines:    rule.LineRange(),
					Reporter: c.Reporter(),
					Text:     fmt.Sprintf("failed to parse pint comment as duration: %s", err),
					Severity: Warning,
				})
			} else {
				minAge = time.Duration(dur)
			}
		}
	}

	return minAge, problems
}

func (c SeriesCheck) isLabelValueIgnored(rule parser.Rule, selector promParser.VectorSelector, labelName string) bool {
	bareSelector := stripLabels(selector)
	for _, s := range []string{
		fmt.Sprintf("rule/set %s ignore/label-value %s", c.Reporter(), labelName),
		fmt.Sprintf("rule/set %s(%s) ignore/label-value %s", c.Reporter(), bareSelector.String(), labelName),
		fmt.Sprintf("rule/set %s(%s) ignore/label-value %s", c.Reporter(), selector.String(), labelName),
	} {
		if rule.HasComment(s) {
			return true
		}
	}
	return false
}

func getSelectors(n *parser.PromQLNode) (selectors []promParser.VectorSelector) {
	if node, ok := n.Node.(*promParser.VectorSelector); ok {
		// copy node without offset
		nc := promParser.VectorSelector{
			Name:          node.Name,
			LabelMatchers: node.LabelMatchers,
		}
		selectors = append(selectors, nc)
	}

	for _, child := range n.Children {
		selectors = append(selectors, getSelectors(child)...)
	}

	return
}

func stripLabels(selector promParser.VectorSelector) promParser.VectorSelector {
	s := promParser.VectorSelector{
		Name:          selector.Name,
		LabelMatchers: []*labels.Matcher{},
	}
	for _, lm := range selector.LabelMatchers {
		if lm.Name == labels.MetricName {
			s.LabelMatchers = append(s.LabelMatchers, lm)
			s.Name = lm.Value
		}
	}
	return s
}

type timeRange struct {
	labels model.LabelSet
	start  time.Time
	end    time.Time
}

type timeRanges struct {
	uri    string
	from   time.Time
	until  time.Time
	step   time.Duration
	ranges []timeRange
}

func (tr timeRanges) withLabelName(name string) (r []timeRange) {
	for _, s := range tr.ranges {
		for k := range s.labels {
			if k == model.LabelName(name) {
				r = append(r, s)
			}
		}
	}
	return
}

func (tr timeRanges) labelValues(name string) (vals []string) {
	vm := map[string]struct{}{}
	for _, s := range tr.ranges {
		for k, v := range s.labels {
			if k == model.LabelName(name) {
				vm[string(v)] = struct{}{}
			}
		}
	}
	for v := range vm {
		vals = append(vals, v)
	}
	return
}

func (tr timeRanges) duration() (d time.Duration) {
	return tr.until.Sub(tr.from)
}

func (tr timeRanges) avgLife() (d time.Duration) {
	for _, r := range tr.ranges {
		d += r.end.Sub(r.start)
	}
	if len(tr.ranges) == 0 {
		return time.Duration(0)
	}
	return time.Second * time.Duration(int(d.Seconds())/len(tr.ranges))
}

func (tr timeRanges) oldest() (ts time.Time) {
	for _, r := range tr.ranges {
		if ts.IsZero() || r.start.Before(ts) {
			ts = r.start
		}
	}
	return
}

func (tr timeRanges) newest() (ts time.Time) {
	for _, r := range tr.ranges {
		if ts.IsZero() || r.end.After(ts) {
			ts = r.end
		}
	}
	return
}

func (tr timeRanges) sinceDesc(t time.Time) (s string) {
	dur := time.Since(t)
	if dur > time.Hour*24 {
		return output.HumanizeDuration(dur.Round(time.Hour))
	}
	return output.HumanizeDuration(dur.Round(time.Minute))
}
