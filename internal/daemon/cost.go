package daemon

import (
	"github.com/worksonmyai/kontora/internal/pricing"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/stats"
	"github.com/worksonmyai/kontora/internal/web"
)

type costHistoryRow struct {
	stage string
	model string
	ended bool
}

type costTicketSnapshot struct {
	id        string
	simpleRun bool
	history   []costHistoryRow
}

type costAccumulator struct {
	total   pricing.Amount
	priced  int
	tracked int
}

// GetTicketCost copies the required ticket fields under d.mu, then reads
// sidecars after it releases the scheduler lock.
func (d *Daemon) GetTicketCost(id string) (web.TicketCostInfo, error) {
	snapshot, err := d.costSnapshot(id)
	if err != nil {
		return web.TicketCostInfo{}, err
	}

	result := web.TicketCostInfo{
		ID:     snapshot.id,
		Stages: []web.TicketCostStageInfo{},
		Runs:   []web.TicketCostRunInfo{},
	}
	logsDir := expandTilde(d.config().LogsDir)
	positions := make(map[string]int)
	stageIndexes := make(map[string]int)
	stageTotals := make([]costAccumulator, 0)
	var ticketTotal costAccumulator

	appendRun := func(historyIndex *int, row costHistoryRow) {
		run := positions[row.stage]
		positions[row.stage] = run + 1

		stageIndex, ok := stageIndexes[row.stage]
		if !ok {
			stageIndex = len(result.Stages)
			stageIndexes[row.stage] = stageIndex
			result.Stages = append(result.Stages, web.TicketCostStageInfo{Name: row.stage})
			stageTotals = append(stageTotals, costAccumulator{})
		}

		info := web.TicketCostRunInfo{
			HistoryIndex: historyIndex,
			Stage:        row.stage,
			Run:          run,
			HistoryModel: row.model,
		}
		var metadata stats.SidecarMetadata
		if session.SafeStage(row.stage) {
			path := statsSidecarPath(logsDir, snapshot.id, row.stage, run)
			if read, found := d.stats.sidecarMetadata(path, row.ended); found {
				metadata = read
				info.SidecarModel = read.Model
			}
		}

		switch {
		case metadata.Model != "":
			info.Model = metadata.Model
			info.ModelSource = "sidecar"
		case row.model != "":
			info.Model = row.model
			info.ModelSource = "history"
		}
		model, amount, priced := calculateRunCost(info.Model, metadata.Usage, pricing.Resolve)
		info.ResolvedModel = model.ID
		if priced {
			value := amount.String()
			info.CostUSD = &value
			stageTotals[stageIndex].add(amount)
			ticketTotal.add(amount)
		}

		stageTotals[stageIndex].tracked++
		ticketTotal.tracked++
		result.Runs = append(result.Runs, info)
	}

	for i, row := range snapshot.history {
		historyIndex := i
		appendRun(&historyIndex, row)
	}
	// A simple execution writes no history row, so its sidecar needs a
	// synthetic run in this response.
	if snapshot.simpleRun {
		appendRun(nil, costHistoryRow{stage: simpleStageName})
	}

	for i := range result.Stages {
		result.Stages[i].PricedRuns = stageTotals[i].priced
		result.Stages[i].TrackedRuns = stageTotals[i].tracked
		result.Stages[i].CostUSD = stageTotals[i].knownCost()
	}
	result.PricedRuns = ticketTotal.priced
	result.TrackedRuns = ticketTotal.tracked
	result.CostUSD = ticketTotal.knownCost()
	return result, nil
}

func (d *Daemon) costSnapshot(id string) (costTicketSnapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	state, ok := d.tickets[id]
	if !ok {
		return costTicketSnapshot{}, web.ErrTicketNotFound
	}
	t := state.ticket
	snapshot := costTicketSnapshot{
		id:        t.ID,
		simpleRun: simpleTicketEnded(t),
		history:   make([]costHistoryRow, len(t.History)),
	}
	for i, row := range t.History {
		snapshot.history[i] = costHistoryRow{
			stage: row.Stage,
			model: row.Model,
			ended: row.CompletedAt != nil,
		}
	}
	return snapshot, nil
}

func calculateRunCost(modelName string, usage *stats.SidecarUsage, resolve func(string) (pricing.Model, bool)) (pricing.Model, pricing.Amount, bool) {
	model, resolved := resolve(modelName)
	if !resolved || usage == nil {
		return model, pricing.Amount{}, false
	}
	amount, priced := pricing.Calculate(model.Pricing, pricing.Usage{
		Input:      usage.Input,
		Output:     usage.Output,
		CacheRead:  usage.CacheRead,
		CacheWrite: usage.CacheCreate,
	})
	return model, amount, priced
}

func (a *costAccumulator) add(amount pricing.Amount) {
	a.total = a.total.Add(amount)
	a.priced++
}

func (a costAccumulator) knownCost() *string {
	if a.priced == 0 {
		return nil
	}
	value := a.total.String()
	return &value
}
