package ticket

import "slices"

// Chain verdicts. They report the dependency graph around a ticket, never the
// ticket's own status: a done ticket whose deps are all closed is ready.
const (
	ChainVerdictReady   = "ready"
	ChainVerdictBlocked = "blocked"
	ChainVerdictCycle   = "cycle"
)

// Where a node sits relative to the ticket the chain was built for.
const (
	ChainUpstream   = "upstream"
	ChainSelf       = "self"
	ChainDownstream = "downstream"
)

// ChainNode is one ticket in a chain, as graph facts alone. Titles and statuses
// are the caller's to fill: this package stays free of presentation, the way
// Classify and Dependents already are.
type ChainNode struct {
	ID    string
	Depth int
	// Direction is upstream, self or downstream.
	Direction      string
	OnCriticalPath bool
	// Resolved is true when the node's status no longer blocks the tickets that
	// depend on it.
	Resolved bool
	// HoldsChain marks the one node the chain is waiting on: an unresolved dep
	// work can start on when something blocks the ticket, and the first
	// unresolved node from the ticket onward when nothing does. No node carries
	// it once the whole chain is resolved.
	HoldsChain bool
	// WaitsOpen and WaitsTotal count the node's own deps, including the ones
	// outside the chain.
	WaitsOpen  int
	WaitsTotal int
	// Missing is set for an id no ticket file answers.
	Missing bool
}

// ChainResult is the dependency chain through one ticket: everything it waits
// on transitively, everything that waits on it transitively, and where it sits
// on the longest path through itself.
type ChainResult struct {
	ID      string
	Verdict string
	// Position is the ticket's 1-based place on the critical path, and Goal is
	// that path's last node. Both are zero on a cycle.
	Position   int
	PathLength int
	Goal       string
	// Total is the true node count. Nodes is capped, so Total > len(Nodes) is
	// how a caller knows the payload was trimmed.
	Total int
	// Done counts the nodes that no longer block, so a cancelled ticket counts
	// with the finished ones: the number drives a progress meter, not a tally
	// of successful runs.
	Done  int
	Cycle []string
	Nodes []ChainNode
}

// Chain walks the dependency closure around id. It reads the whole index, so
// the caller must pass every ticket on disk and not only the visible ones.
// nodeCap bounds Nodes; the critical path is kept whatever the cap. A cap of
// zero or less does not trim.
//
// An id with no ticket behind it returns an empty chain rather than a graph
// rooted at nothing.
func Chain(index map[string]*Ticket, id string, nodeCap int) ChainResult {
	res := ChainResult{ID: id, Verdict: ChainVerdictReady, Cycle: []string{}, Nodes: []ChainNode{}}
	self, ok := index[id]
	if !ok {
		return res
	}

	rev := reverseDeps(index)
	deps := func(n string) []string {
		t, ok := index[n]
		if !ok {
			return nil
		}
		return t.Deps
	}

	// The node set: what self waits on, self, and what waits on self. Not the
	// undirected neighbourhood — a sibling sharing a dep with self is neither.
	dir := map[string]string{}
	for n := range closure(id, deps) {
		dir[n] = ChainUpstream
	}
	for n := range closure(id, func(n string) []string { return rev[n] }) {
		dir[n] = ChainDownstream
	}
	dir[id] = ChainSelf

	// Deps inside the set, deduplicated: a repeated id would otherwise count
	// twice against the in-degree and stall Kahn's algorithm.
	depsIn := func(n string) []string {
		var out []string
		for _, d := range deps(n) {
			if d == "" {
				continue
			}
			if _, in := dir[d]; in && !slices.Contains(out, d) {
				out = append(out, d)
			}
		}
		return out
	}
	dependentsIn := func(n string) []string {
		var out []string
		for _, d := range rev[n] {
			if _, in := dir[d]; in {
				out = append(out, d)
			}
		}
		return out
	}

	depth, residual := kahnDepths(dir, depsIn, dependentsIn)
	if len(residual) > 0 {
		res.Verdict = ChainVerdictCycle
		res.Cycle = residualCycle(residual, id)
		return res
	}

	// Height is depth over the reversed subgraph: the longest distance to a
	// sink. The forward half of the walk has to rank on it, because depth says
	// how much chain is behind a node and nothing about what is left ahead.
	height, _ := kahnDepths(dir, dependentsIn, depsIn)

	resolved := func(n string) bool {
		t, ok := index[n]
		return ok && IsDependencyResolved(t.Status)
	}
	// Greatest rank wins; on a tie the unresolved node, because a resolved dep
	// contributes nothing to when the goal can start; then the lowest id.
	rankedBy := func(rank map[string]int) func(a, b string) bool {
		return func(a, b string) bool {
			if rank[a] != rank[b] {
				return rank[a] > rank[b]
			}
			if ra, rb := resolved(a), resolved(b); ra != rb {
				return !ra
			}
			return a < b
		}
	}
	path := criticalPath(id, depsIn, dependentsIn, rankedBy(depth), rankedBy(height))

	res.Position = slices.Index(path, id) + 1
	res.PathLength = len(path)
	res.Goal = path[len(path)-1]
	blocked := !Classify(self, index).Ready
	if blocked {
		res.Verdict = ChainVerdictBlocked
	}
	holder := chainHolder(index, self, blocked, path, resolved, rankedBy(depth))

	seq := discoverySeq(index, rev, dir, id)
	for n, d := range dir {
		t, known := index[n]
		node := ChainNode{
			ID:             n,
			Depth:          depth[n],
			Direction:      d,
			OnCriticalPath: slices.Contains(path, n),
			Resolved:       resolved(n),
			HoldsChain:     n == holder,
			Missing:        !known,
		}
		if known {
			for _, dep := range t.Deps {
				if dep == "" {
					continue
				}
				node.WaitsTotal++
				if !resolved(dep) {
					node.WaitsOpen++
				}
			}
		}
		res.Nodes = append(res.Nodes, node)
		res.Total++
		if resolved(n) {
			res.Done++
		}
	}
	sortChainNodes(res.Nodes, seq)
	res.Nodes = capChainNodes(res.Nodes, nodeCap)
	return res
}

// reverseDeps maps a ticket id to the ids that name it in deps, sorted. One
// pass over the index: Dependents is O(V) per node, and the whole walk runs
// under the daemon lock.
func reverseDeps(index map[string]*Ticket) map[string][]string {
	rev := map[string][]string{}
	for id, t := range index {
		for _, dep := range t.Deps {
			if dep == "" || slices.Contains(rev[dep], id) {
				continue
			}
			rev[dep] = append(rev[dep], id)
		}
	}
	for _, ids := range rev {
		slices.Sort(ids)
	}
	return rev
}

// closure returns everything reachable from start along next, excluding start
// unless a cycle leads back to it.
func closure(start string, next func(string) []string) map[string]bool {
	seen := map[string]bool{}
	queue := next(start)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		queue = append(queue, next(cur)...)
	}
	return seen
}

// kahnDepths computes each node's longest distance from a root over the induced
// subgraph. The residual maps every node Kahn could not settle to the deps that
// held it back, which is non-empty exactly when the subgraph has a cycle.
func kahnDepths(dir map[string]string, depsIn, dependentsIn func(string) []string) (depth map[string]int, residual map[string][]string) {
	depth = map[string]int{}
	indeg := map[string]int{}
	var queue []string
	for n := range dir {
		indeg[n] = len(depsIn(n))
		if indeg[n] == 0 {
			queue = append(queue, n)
		}
	}
	slices.Sort(queue)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		delete(indeg, cur)
		for _, next := range dependentsIn(cur) {
			if depth[cur]+1 > depth[next] {
				depth[next] = depth[cur] + 1
			}
			indeg[next]--
			if indeg[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if len(indeg) == 0 {
		return depth, nil
	}
	residual = map[string][]string{}
	for n := range indeg {
		for _, d := range depsIn(n) {
			if _, stuck := indeg[d]; stuck {
				residual[n] = append(residual[n], d)
			}
		}
		slices.Sort(residual[n])
	}
	return depth, residual
}

// residualCycle names one cycle in Kahn's leftovers. Every residual node has at
// least one residual dep, so following deps never dead-ends and has to repeat a
// node. It starts at the requested ticket when that ticket is on the residual,
// so a cycle through it reads from it, and at the lowest id otherwise.
func residualCycle(residual map[string][]string, start string) []string {
	if _, ok := residual[start]; !ok {
		start = ""
		for n := range residual {
			if start == "" || n < start {
				start = n
			}
		}
	}
	at := map[string]int{}
	var path []string
	for cur := start; ; cur = residual[cur][0] {
		if i, seen := at[cur]; seen {
			return append(slices.Clone(path[i:]), cur)
		}
		at[cur] = len(path)
		path = append(path, cur)
	}
}

// criticalPath is the longest path through the ticket: back through its deps to
// a root, then forward through its dependents to the goal. The two halves rank
// candidates differently, backward on depth and forward on height.
func criticalPath(id string, depsIn, dependentsIn func(string) []string, backward, forward func(a, b string) bool) []string {
	pick := func(cands []string, better func(a, b string) bool) string {
		best := ""
		for _, c := range cands {
			if best == "" || better(c, best) {
				best = c
			}
		}
		return best
	}
	path := []string{id}
	for cur := pick(depsIn(id), backward); cur != ""; cur = pick(depsIn(cur), backward) {
		path = append([]string{cur}, path...)
	}
	for cur := pick(dependentsIn(id), forward); cur != ""; cur = pick(dependentsIn(cur), forward) {
		path = append(path, cur)
	}
	return path
}

// chainHolder names the node the chain is waiting on. While something blocks
// the ticket that is an unresolved blocker upstream with no unresolved dep of
// its own, so the node named is one work can actually start on: sitting on the
// critical path neither makes a node a blocker nor keeps it from being one.
// While nothing blocks the ticket it is the first unresolved node from the
// ticket onward, which is the ticket itself until the ticket closes.
func chainHolder(index map[string]*Ticket, self *Ticket, blocked bool, path []string, resolved func(string) bool, better func(a, b string) bool) string {
	if !blocked {
		// The ticket is always on its own path, so the scan starts at it.
		for _, n := range path[slices.Index(path, self.ID):] {
			if !resolved(n) {
				return n
			}
		}
		return ""
	}
	// Two blockers work could start on are ranked as the walk ranks a fork: the
	// critical path first, then the deeper node, then the lowest id.
	betterHolder := func(a, b string) bool {
		if pa, pb := slices.Contains(path, a), slices.Contains(path, b); pa != pb {
			return pa
		}
		return better(a, b)
	}
	best := ""
	seen := map[string]bool{self.ID: true}
	queue := []string{self.ID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		var blockers []string
		if t, ok := index[cur]; ok {
			blockers = Classify(t, index).Blockers
		}
		// Nothing unresolved behind it, so this is where work starts. An id with
		// no ticket file lands here too: it has no dep to walk past, and there
		// is no way to tell that work is finished.
		if len(blockers) == 0 {
			if best == "" || betterHolder(cur, best) {
				best = cur
			}
			continue
		}
		for _, b := range blockers {
			if !seen[b] {
				seen[b] = true
				queue = append(queue, b)
			}
		}
	}
	return best
}

// discoverySeq numbers the node set by a breadth-first walk out of the ticket,
// deps in the order the frontmatter lists them and dependents by id. It is the
// tie-break within a depth layer, so a row of sibling deps reads in the order
// the ticket that waits on them wrote them.
func discoverySeq(index map[string]*Ticket, rev map[string][]string, dir map[string]string, start string) map[string]int {
	seq := map[string]int{start: 0}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		var next []string
		if t, ok := index[cur]; ok {
			next = append(next, t.Deps...)
		}
		next = append(next, rev[cur]...)
		for _, n := range next {
			if _, in := dir[n]; !in {
				continue
			}
			if _, seen := seq[n]; seen {
				continue
			}
			seq[n] = len(seq)
			queue = append(queue, n)
		}
	}
	return seq
}

func sortChainNodes(nodes []ChainNode, seq map[string]int) {
	slices.SortFunc(nodes, func(a, b ChainNode) int {
		if a.Depth != b.Depth {
			return a.Depth - b.Depth
		}
		return seq[a.ID] - seq[b.ID]
	})
}

// capChainNodes trims the node list to limit, keeping the critical path and the
// holder whatever the limit: a chain too big to draw still has to say what is
// holding it up, and the holder is not always on the path. The surviving nodes
// stay in the order they came in.
func capChainNodes(nodes []ChainNode, limit int) []ChainNode {
	if limit <= 0 || len(nodes) <= limit {
		return nodes
	}
	keep := map[string]bool{}
	for _, n := range nodes {
		if n.OnCriticalPath || n.HoldsChain {
			keep[n.ID] = true
		}
	}
	for _, n := range nodes {
		if len(keep) >= limit {
			break
		}
		keep[n.ID] = true
	}
	kept := make([]ChainNode, 0, len(keep))
	for _, n := range nodes {
		if keep[n.ID] {
			kept = append(kept, n)
		}
	}
	return kept
}
