package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"mediamerge/internal/checksum"
	"mediamerge/internal/demux"
	"mediamerge/internal/mapping"
	"mediamerge/internal/model"
	"mediamerge/internal/txn"
)

// runMerge remuxes an arbitrary input file with explicit mapping rules.
func runMerge(args []string) error {
	fs := newFlagSet("merge")
	input := fs.String("input", "", "input MP4/MOV file")
	output := fs.String("output", "", "target MP4 file")
	mapSpec := fs.String("map", "", `mapping rules, e.g. "1=10:video,2=11:audio"`)
	queue := fs.Int("queue", 8, "bounded sample queue capacity")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *input == "" || *output == "" {
		fs.Usage()
		return model.NewError(model.StageVerify, model.CodePath, -1, 0, false,
			"--input and --output are required")
	}
	ctx := context.Background()
	r, err := demux.Open(ctx, *input)
	if err != nil {
		return err
	}
	defer r.Close()
	rules, err := mapping.ParseRules(*mapSpec)
	if err != nil {
		return err
	}
	plan, err := mapping.Apply(r.Tracks, rules)
	if err != nil {
		return err
	}
	res, err := txn.Merge(ctx, filepath.Dir(*output), *output, r, plan, txn.Options{Queue: *queue, InterruptAfter: -1})
	if err != nil {
		return err
	}
	fmt.Println(res.FormatSummary())
	return nil
}

// runSummarize prints mapping, sample counts, time ranges and digests.
func runSummarize(args []string) error {
	fs := newFlagSet("summarize")
	dir := fs.String("dir", "", "work directory")
	jsonOut := fs.Bool("json", false, "emit the summary as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireDir(fs, *dir); err != nil {
		return err
	}
	ctx := context.Background()
	base, err := readInputPlan(ctx, filepath.Join(*dir, "input.mp4"))
	if err != nil {
		return err
	}
	defer base.reader.Close()
	output, _ := txn.Paths(*dir)
	rules := mapping.DefaultRules(base.reader.Tracks)
	plan, err := mapping.Apply(base.reader.Tracks, rules)
	if err != nil {
		return err
	}
	out, err := demux.Open(ctx, output)
	if err != nil {
		return err
	}
	defer out.Close()
	var inputs []txn.TrackSummaryInput
	for _, t := range plan.Tracks {
		inputs = append(inputs, txn.TrackSummaryInput{
			InputID: t.InputID, TargetID: t.TargetID, Media: t.MediaType, Codec: t.Codec,
			Timescale: t.Timescale, Samples: len(t.Samples), FirstDTS: t.FirstDTS, LastDTS: t.LastDTS,
		})
	}
	inSHA, inSize, err := checksum.File(base.reader.Path)
	if err != nil {
		return err
	}
	outSHA, outSize, err := checksum.File(output)
	if err != nil {
		return err
	}
	_ = inSize
	digests := make([]txn.TrackDigest, 0, len(plan.Tracks))
	match := true
	for i, t := range plan.Tracks {
		a := checksum.NewSampleHasher()
		for _, ref := range t.Samples {
			data, err := base.reader.ReadSample(ref)
			if err != nil {
				return err
			}
			a.Add(data)
		}
		b := checksum.NewSampleHasher()
		for _, ref := range out.Tracks[i].Samples {
			data, err := out.ReadSample(ref)
			if err != nil {
				return err
			}
			b.Add(data)
		}
		d := txn.TrackDigest{TargetID: t.TargetID, Samples: len(t.Samples),
			InputHash: a.Sum(), OutputHash: b.Sum(), Match: a.Sum() == b.Sum()}
		match = match && d.Match
		digests = append(digests, d)
	}
	res := &txn.Result{InputSHA: inSHA, OutputSHA: outSHA, Samples: 0, Tracks: len(plan.Tracks),
		SampleMatch: match, Digests: digests, OutputLen: outSize}
	for _, t := range out.Tracks {
		res.Samples += len(t.Samples)
	}
	summary, err := txn.NewSummary(res, inputs)
	if err != nil {
		return err
	}
	if *jsonOut {
		fmt.Println(summary.JSON())
	} else {
		fmt.Print(summary.Text())
	}
	return nil
}

// runPlan prints the validated mapping without writing media.
func runPlan(args []string) error {
	fs := newFlagSet("plan")
	input := fs.String("input", "", "input MP4/MOV file")
	mapSpec := fs.String("map", "", "mapping rules")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *input == "" {
		fs.Usage()
		return model.NewError(model.StageVerify, model.CodePath, -1, 0, false, "--input is required")
	}
	r, err := demux.Open(context.Background(), *input)
	if err != nil {
		return err
	}
	defer r.Close()
	rules, err := mapping.ParseRules(*mapSpec)
	if err != nil {
		return err
	}
	plan, err := mapping.Apply(r.Tracks, rules)
	if err != nil {
		return err
	}
	for _, t := range plan.Tracks {
		fmt.Printf("track input=%d target=%d media=%s codec=%s timescale=%d samples=%d\n",
			t.InputID, t.TargetID, t.MediaType, t.Codec, t.Timescale, len(t.Samples))
	}
	return nil
}

type inputPlan struct {
	reader *demux.Reader
	plan   *mapping.Plan
}

func readInputPlan(ctx context.Context, input string) (*inputPlan, error) {
	r, err := demux.Open(ctx, input)
	if err != nil {
		return nil, err
	}
	plan, err := mapping.Apply(r.Tracks, mapping.DefaultRules(r.Tracks))
	if err != nil {
		r.Close()
		return nil, err
	}
	return &inputPlan{reader: r, plan: plan}, nil
}

// crashAfter runs the pipeline and stops after the requested stage, leaving a
// journal plus an intentionally uncommitted tail on disk.
func crashAfter(ctx context.Context, dir string, base *inputPlan, stage, queue int) error {
	_, err := txn.Run(ctx, dir, base.reader, base.plan, txn.Options{
		Queue: queue, InterruptAfter: stage, CrashBytes: -1,
	})
	if errors.Is(err, txn.ErrInterrupted) {
		return nil
	}
	if err != nil {
		return err
	}
	return model.NewError(model.StageRecovery, model.CodeIntegrity, -1, 0, false,
		"interrupt-after=%d did not interrupt the run", stage)
}

func nowMS() int64 { return time.Now().UnixNano() / 1e6 }
