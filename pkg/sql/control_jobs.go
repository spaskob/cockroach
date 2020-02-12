// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/errors"
)

type controlJobsNode struct {
	rows          planNode
	desiredStatus jobs.Status
	numRows       int
}

var jobCommandToDesiredStatus = map[tree.JobCommand]jobs.Status{
	tree.CancelJob: jobs.StatusCanceled,
	tree.ResumeJob: jobs.StatusRunning,
	tree.PauseJob:  jobs.StatusPaused,
}

// FastPathResults implements the planNodeFastPath inteface.
func (n *controlJobsNode) FastPathResults() (int, bool) {
	return n.numRows, true
}

func (n *controlJobsNode) startExec(params runParams) error {
	reg := params.p.ExecCfg().JobRegistry
	for {
		ok, err := n.rows.Next(params)
		if err != nil {
			return err
		}
		if !ok {
			break
		}

		jobIDDatum := n.rows.Values()[0]
		if jobIDDatum == tree.DNull {
			continue
		}

		jobID, ok := tree.AsDInt(jobIDDatum)
		if !ok {
			return errors.AssertionFailedf("%q: expected *DInt, found %T", jobIDDatum, jobIDDatum)
		}

		switch n.desiredStatus {
		case jobs.StatusPaused:
			err = reg.PauseRequested(params.ctx, params.p.txn, int64(jobID))
			if err != nil {
				break
			}
			log.Infof(params.ctx, "job %d: pause requested, waiting to be paused", int64(jobID))
			// PauseRequested does not actually pause the job but only sends a
			// request for it. Actually block until the job state changes.
			opts := retry.Options{
				InitialBackoff: 4 * time.Second,
				MaxBackoff:     time.Minute,
				Multiplier:     2,
			}
			err = retry.WithMaxAttempts(params.ctx, opts, 10, func() error {
				row, err := params.p.ExecCfg().InternalExecutor.QueryRowEx(
					params.ctx,
					"job-status",
					params.p.txn,
					sqlbase.InternalExecutorSessionDataOverride{User: security.RootUser},
					`SELECT status FROM system.jobs WHERE id = $1`,
					int64(jobID))
				if err != nil {
					return err
				}
				statusString := tree.MustBeDString(row[0])
				if jobs.Status(statusString) != jobs.StatusPaused {
					return errors.Errorf("job %id: timed out waiting to be paused", int64(jobID))
				}
				return nil
			})
			if err != nil {
				log.Error(params.ctx, "%v", err)
			}
			log.Infof(params.ctx, "job %d: paused", int64(jobID))
		case jobs.StatusRunning:
			err = reg.Resume(params.ctx, params.p.txn, int64(jobID))
		case jobs.StatusCanceled:
			err = reg.CancelRequested(params.ctx, params.p.txn, int64(jobID))
		default:
			err = errors.AssertionFailedf("unhandled status %v", n.desiredStatus)
		}
		if err != nil {
			return err
		}
		n.numRows++
	}
	return nil
}

func (*controlJobsNode) Next(runParams) (bool, error) { return false, nil }

func (*controlJobsNode) Values() tree.Datums { return nil }

func (n *controlJobsNode) Close(ctx context.Context) {
	n.rows.Close(ctx)
}
