// Package model holds the types shared between collection, classification and rendering.
package model

import "time"

type Kind string

const (
	KindStuckPR         Kind = "stuck-pr"
	KindNeedsReply      Kind = "needs-reply"
	KindRedWorkflow     Kind = "red-workflow"
	KindRetryInProgress Kind = "retry-in-progress"
	KindCannotRead      Kind = "cannot-read"
)

type Finding struct {
	Repo  string    `json:"repo"`
	Item  string    `json:"item"`
	URL   string    `json:"url"`
	Kind  Kind      `json:"kind"`
	Since time.Time `json:"since"`
	Why   string    `json:"why"`
	Fix   string    `json:"fix"`
}
