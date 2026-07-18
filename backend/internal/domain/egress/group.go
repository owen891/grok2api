package egress

import "time"

type GroupStrategy string

const (
	StrategyLeastLoad  GroupStrategy = "least_load"
	StrategyWeighted   GroupStrategy = "weighted"
	StrategySticky     GroupStrategy = "sticky"
	StrategyRoundRobin GroupStrategy = "round_robin"
)

type Group struct {
	ID              uint64
	Name            string
	Scope           Scope
	Enabled         bool
	Strategy        GroupStrategy
	MaxConcurrency  int
	FallbackGroupID *uint64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type GroupMember struct {
	GroupID        uint64
	NodeID         uint64
	Weight         int
	MaxConcurrency int
	Enabled        bool
	Priority       int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
