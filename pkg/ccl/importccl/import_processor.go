// Copyright 2017 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package importccl

import (
	"context"
	"math"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/row"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/storage/storagebase"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
)

var csvOutputTypes = []types.T{
	*types.Bytes,
	*types.Bytes,
}

type readImportDataProcessor struct {
	flowCtx *execinfra.FlowCtx
	spec    execinfrapb.ReadImportDataSpec
	output  execinfra.RowReceiver
}

var _ execinfra.Processor = &readImportDataProcessor{}

func (cp *readImportDataProcessor) OutputTypes() []types.T {
	return csvOutputTypes
}

func newReadImportDataProcessor(
	flowCtx *execinfra.FlowCtx,
	processorID int32,
	spec execinfrapb.ReadImportDataSpec,
	output execinfra.RowReceiver,
) (execinfra.Processor, error) {
	cp := &readImportDataProcessor{
		flowCtx: flowCtx,
		spec:    spec,
		output:  output,
	}
	return cp, nil
}

func (cp *readImportDataProcessor) Run(ctx context.Context) {
	ctx, span := tracing.ChildSpan(ctx, "readImportDataProcessor")
	defer tracing.FinishSpan(span)
	defer cp.output.ProducerDone()

	kvCh := make(chan row.KVBatch, 10)
	progCh := make(chan execinfrapb.RemoteProducerMetadata_BulkProcessorProgress)

	var summary *roachpb.BulkOpSummary
	var err error
	// We don't have to worry about this go routine leaking because next we loop over progCh
	// which is closed only after the go routine returns.
	go func() {
		defer close(progCh)
		summary, err = runImport(ctx, cp.flowCtx, &cp.spec, progCh, kvCh, cp.output)
	}()

	for prog := range progCh {
		cp.output.Push(nil, &execinfrapb.ProducerMetadata{BulkProcessorProgress: &prog})
	}


	if err != nil {
		cp.output.Push(nil, &execinfrapb.ProducerMetadata{Err: err})
		return
	}

	// Once the import is done, send back to the controller the serialized
	// summary of the import operation. For more info see roachpb.BulkOpSummary.
	if summary == nil {
		return
	}
	countsBytes, err := protoutil.Marshal(summary)
	if err != nil {
		cp.output.Push(nil, &execinfrapb.ProducerMetadata{Err: err})
		return
	}
	cp.output.Push(sqlbase.EncDatumRow{
		sqlbase.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(countsBytes))),
		sqlbase.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes([]byte{}))),
	}, nil)
}

func makeInputConverter(
	spec *execinfrapb.ReadImportDataSpec, evalCtx *tree.EvalContext, kvCh chan row.KVBatch,
) (inputConverter, error) {

	var singleTable *sqlbase.TableDescriptor
	var singleTableTargetCols tree.NameList
	if len(spec.Tables) == 1 {
		for _, table := range spec.Tables {
			singleTable = table.Desc
			singleTableTargetCols = make(tree.NameList, len(table.TargetCols))
			for i, colName := range table.TargetCols {
				singleTableTargetCols[i] = tree.Name(colName)
			}
		}
	}

	if format := spec.Format.Format; singleTable == nil && !isMultiTableFormat(format) {
		return nil, errors.Errorf("%s only supports reading a single, pre-specified table", format.String())
	}

	switch spec.Format.Format {
	case roachpb.IOFileFormat_CSV:
		isWorkload := true
		for _, file := range spec.Uri {
			if conf, err := storageccl.ExportStorageConfFromURI(file); err != nil || conf.Provider != roachpb.ExportStorageProvider_Workload {
				isWorkload = false
				break
			}
		}
		if isWorkload {
			return newWorkloadReader(kvCh, singleTable, evalCtx), nil
		}
		return newCSVInputReader(kvCh, spec.Format.Csv, spec.WalltimeNanos, singleTable, singleTableTargetCols, evalCtx), nil
	case roachpb.IOFileFormat_MysqlOutfile:
		return newMysqloutfileReader(kvCh, spec.Format.MysqlOut, singleTable, evalCtx)
	case roachpb.IOFileFormat_Mysqldump:
		return newMysqldumpReader(kvCh, spec.Tables, evalCtx)
	case roachpb.IOFileFormat_PgCopy:
		return newPgCopyReader(kvCh, spec.Format.PgCopy, singleTable, evalCtx)
	case roachpb.IOFileFormat_PgDump:
		return newPgDumpReader(kvCh, spec.Format.PgDump, spec.Tables, evalCtx)
	default:
		return nil, errors.Errorf("Requested IMPORT format (%d) not supported by this node", spec.Format.Format)
	}
}

type sampleFunc func(roachpb.KeyValue) bool

// sampleRate is a sampleFunc that samples a row with a probability of the
// row's size / the sample size.
type sampleRate struct {
	rnd        *rand.Rand
	sampleSize float64
}

func (s sampleRate) sample(kv roachpb.KeyValue) bool {
	sz := float64(len(kv.Key) + len(kv.Value.RawBytes))
	prob := sz / s.sampleSize
	return prob > s.rnd.Float64()
}

func emitKvs(
	flowCtx *execinfra.FlowCtx,
	spec *execinfrapb.ReadImportDataSpec,
	ctx context.Context,
	kvCh <-chan row.KVBatch,
	output execinfra.RowReceiver,
) error {
	ctx, span := tracing.ChildSpan(ctx, "sendImportKVs")
	defer tracing.FinishSpan(span)

	var fn sampleFunc
	var sampleAll bool
	if spec.SampleSize == 0 {
		sampleAll = true
	} else {
		sr := sampleRate{
			rnd:        rand.New(rand.NewSource(rand.Int63())),
			sampleSize: float64(spec.SampleSize),
		}
		fn = sr.sample
	}

	// Populate the split-point spans which have already been imported.
	var completedSpans roachpb.SpanGroup
	job, err := flowCtx.Cfg.JobRegistry.LoadJob(ctx, spec.Progress.JobID)
	if err != nil {
		return err
	}
	progress := job.Progress()
	if details, ok := progress.Details.(*jobspb.Progress_Import); ok {
		completedSpans.Add(details.Import.SpanProgress...)
	} else {
		return errors.Errorf("unexpected progress type %T", progress)
	}

	for kvBatch := range kvCh {
		for _, kv := range kvBatch.KVs {
			// Allow KV pairs to be dropped if they belong to a completed span.
			if completedSpans.Contains(kv.Key) {
				continue
			}

			rowRequired := sampleAll || keys.IsDescriptorKey(kv.Key)
			if rowRequired || fn(kv) {
				var row sqlbase.EncDatumRow
				if rowRequired {
					row = sqlbase.EncDatumRow{
						sqlbase.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(kv.Key))),
						sqlbase.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(kv.Value.RawBytes))),
					}
				} else {
					// Don't send the value for rows returned for sampling
					row = sqlbase.EncDatumRow{
						sqlbase.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(kv.Key))),
						sqlbase.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes([]byte{}))),
					}
				}
				if output.Push(row, nil) != execinfra.NeedMoreRows {
					return errors.New("unexpected closure of consumer")
				}
			}
		}
	}
	return nil
}

// ingestKvs drains kvs from the channel until it closes, ingesting them using
// the BulkAdder. It handles the required buffering/sorting/etc.
func ingestKvs(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	spec *execinfrapb.ReadImportDataSpec,
	progCh chan execinfrapb.RemoteProducerMetadata_BulkProcessorProgress,
	kvCh <-chan row.KVBatch,
) (*roachpb.BulkOpSummary, error) {
	ctx, span := tracing.ChildSpan(ctx, "ingestKVs")
	defer tracing.FinishSpan(span)

	writeTS := hlc.Timestamp{WallTime: spec.WalltimeNanos}

	flushSize := storageccl.MaxImportBatchSize(flowCtx.Cfg.Settings)

	// We create two bulk adders so as to combat the excessive flushing of small
	// SSTs which was observed when using a single adder for both primary and
	// secondary index kvs. The number of secondary index kvs are small, and so we
	// expect the indexAdder to flush much less frequently than the pkIndexAdder.
	//
	// It is highly recommended that the cluster setting controlling the max size
	// of the pkIndexAdder buffer be set below that of the indexAdder buffer.
	// Otherwise, as a consequence of filling up faster the pkIndexAdder buffer
	// will hog memory as it tries to grow more aggressively.
	minBufferSize, maxBufferSize, stepSize := storageccl.ImportBufferConfigSizes(flowCtx.Cfg.Settings, true /* isPKAdder */)
	pkIndexAdder, err := flowCtx.Cfg.BulkAdder(ctx, flowCtx.Cfg.DB, writeTS, storagebase.BulkAdderOptions{
		Name:              "pkAdder",
		DisallowShadowing: true,
		SkipDuplicates:    true,
		MinBufferSize:     uint64(minBufferSize),
		MaxBufferSize:     uint64(maxBufferSize),
		StepBufferSize:    uint64(stepSize),
		SSTSize:           uint64(flushSize),
	})
	if err != nil {
		return nil, err
	}
	defer pkIndexAdder.Close(ctx)

	minBufferSize, maxBufferSize, stepSize = storageccl.ImportBufferConfigSizes(flowCtx.Cfg.Settings, false /* isPKAdder */)
	indexAdder, err := flowCtx.Cfg.BulkAdder(ctx, flowCtx.Cfg.DB, writeTS, storagebase.BulkAdderOptions{
		Name:              "indexAdder",
		DisallowShadowing: true,
		SkipDuplicates:    true,
		MinBufferSize:     uint64(minBufferSize),
		MaxBufferSize:     uint64(maxBufferSize),
		StepBufferSize:    uint64(stepSize),
		SSTSize:           uint64(flushSize),
	})
	if err != nil {
		return nil, err
	}
	defer indexAdder.Close(ctx)

	// Setup progress tracking:
	//  - offsets maps source file IDs to offsets in the slices below.
	//  - writtenRow contains LastRow of batch most recently added to the buffer.
	//  - writtenFraction contains % of the input finished as of last batch.
	//  - pkFlushedRow contains `writtenRow` as of the last pk adder flush.
	//  - idxFlushedRow contains `writtenRow` as of the last index adder flush.
	// In pkFlushedRow, idxFlushedRow and writtenFaction values are written via
	// `atomic` so the progress reporting go goroutine can read them.
	writtenRow := make([]uint64, len(spec.Uri))
	writtenFraction := make([]uint32, len(spec.Uri))

	pkFlushedRow := make([]uint64, len(spec.Uri))
	idxFlushedRow := make([]uint64, len(spec.Uri))

	// When the PK adder flushes, everything written has been flushed, so we set
	// pkFlushedRow to writtenRow. Additionally if the indexAdder is empty then we
	// can treat it as flushed as well (in case we're not adding anything to it).
	pkIndexAdder.SetOnFlush(func() {
		for _, i := range writtenRow {
			atomic.StoreUint64(&pkFlushedRow[i], writtenRow[i])
		}
		if indexAdder.IsEmpty() {
			for _, i := range writtenRow {
				atomic.StoreUint64(&idxFlushedRow[i], writtenRow[i])
			}
		}
	})
	indexAdder.SetOnFlush(func() {
		for _, i := range writtenRow {
			atomic.StoreUint64(&idxFlushedRow[i], writtenRow[i])
		}
	})

	// offsets maps input file ID to a slot in our progress tracking slices.
	offsets := make(map[int32]int, len(spec.Uri))
	var offset int
	for i := range spec.Uri {
		offsets[i] = offset
		offset++
	}

	// stopProgress will be closed when there is no more progress to report.
	stopProgress := make(chan struct{})
	g := ctxgroup.WithContext(ctx)
	g.GoCtx(func(ctx context.Context) error {
		tick := time.NewTicker(time.Second * 10)
		defer tick.Stop()
		done := ctx.Done()
		for {
			select {
			case <-done:
				return ctx.Err()
			case <-stopProgress:
				return nil
			case <-tick.C:
				var prog execinfrapb.RemoteProducerMetadata_BulkProcessorProgress
				prog.CompletedRow = make(map[int32]uint64)
				prog.CompletedFraction = make(map[int32]float32)
				for file, offset := range offsets {
					pk := atomic.LoadUint64(&pkFlushedRow[offset])
					idx := atomic.LoadUint64(&idxFlushedRow[offset])
					// On resume we'll be able to skip up the last row for which both the
					// PK and index adders have flushed KVs.
					if idx > pk {
						prog.CompletedRow[file] = pk
					} else {
						prog.CompletedRow[file] = idx
					}
					prog.CompletedFraction[file] = math.Float32frombits(atomic.LoadUint32(&writtenFraction[offset]))
				}
				progCh <- prog
			}
		}
	})

	g.GoCtx(func(ctx context.Context) error {
		defer close(stopProgress)

		// We insert splits at every index span of the table above. Since the
		// BulkAdder is split aware when constructing SSTs, there is no risk of worst
		// case overlap behavior in the resulting AddSSTable calls.
		//
		// NB: We are getting rid of the pre-buffering stage which constructed
		// separate buckets for each table's primary data, and flushed to the
		// BulkAdder when the bucket was full. This is because, a tpcc 1k IMPORT would
		// OOM when maintaining this buffer. Two big wins we got from this
		// pre-buffering stage were:
		//
		// 1. We avoided worst case overlapping behavior in the AddSSTable calls as a
		// result of flushing keys with the same TableIDIndexID prefix, together.
		//
		// 2. Secondary index KVs which were few and filled the bucket infrequently
		// were flushed rarely, resulting in fewer L0 (and total) files.
		//
		// While we continue to achieve the first property as a result of the splits
		// mentioned above, the KVs sent to the BulkAdder are no longer grouped which
		// results in flushing a much larger number of small SSTs. This increases the
		// number of L0 (and total) files, but with a lower memory usage.
		for kvBatch := range kvCh {
			for _, kv := range kvBatch.KVs {
				_, _, indexID, indexErr := sqlbase.DecodeTableIDIndexID(kv.Key)
				if indexErr != nil {
					return indexErr
				}

				// Decide which adder to send the KV to by extracting its index id.
				//
				// TODO(adityamaru): There is a potential optimization of plumbing the
				// different putters, and differentiating based on their type. It might be
				// more efficient than parsing every kv.
				if indexID == 1 {
					if err := pkIndexAdder.Add(ctx, kv.Key, kv.Value.RawBytes); err != nil {
						if _, ok := err.(storagebase.DuplicateKeyError); ok {
							return errors.Wrap(err, "duplicate key in primary index")
						}
						return err
					}
				} else {
					if err := indexAdder.Add(ctx, kv.Key, kv.Value.RawBytes); err != nil {
						if _, ok := err.(storagebase.DuplicateKeyError); ok {
							return errors.Wrap(err, "duplicate key in index")
						}
						return err
					}
				}
			}
			offset := offsets[kvBatch.Source]
			writtenRow[offset] = kvBatch.LastRow
			atomic.StoreUint32(&writtenFraction[offset], math.Float32bits(kvBatch.Progress))
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	if err := pkIndexAdder.Flush(ctx); err != nil {
		if err, ok := err.(storagebase.DuplicateKeyError); ok {
			return nil, errors.Wrap(err, "duplicate key in primary index")
		}
		return nil, err
	}

	if err := indexAdder.Flush(ctx); err != nil {
		if err, ok := err.(storagebase.DuplicateKeyError); ok {
			return nil, errors.Wrap(err, "duplicate key in index")
		}
		return nil, err
	}

	addedSummary := pkIndexAdder.GetSummary()
	addedSummary.Add(indexAdder.GetSummary())
	return &addedSummary, nil
}

func init() {
	rowexec.NewReadImportDataProcessor = newReadImportDataProcessor
}
