package orchestrator

// PlannerEvidence retains the records a tool-disabled planner needs to interpret outcomes.
type PlannerEvidence struct {
	Attempts  []Attempt       `json:"attempts"`
	Results   []PlannerResult `json:"results"`
	Questions []Question      `json:"questions"`
	Wakes     []PlannerWake   `json:"wakes"`
}

type PlannerResult struct {
	Result
	Information []PlannerInformation `json:"information,omitempty"`
}

type PlannerInformation struct {
	Artifact  ArtifactRef `json:"artifact"`
	Text      string      `json:"text,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
	Binary    bool        `json:"binary,omitempty"`
}

type PlannerWake struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
