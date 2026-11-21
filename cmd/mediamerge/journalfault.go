package main

import (
	"context"
	"errors"
	"fmt"

	"mediamerge/internal/journal"
	"mediamerge/internal/model"
	"mediamerge/internal/txn"
	"mediamerge/internal/verify"
)

// corruptJournal rewrites the last commit record with a wrong prefix digest,
// producing the "journal and artifact disagree" fault class. It then runs the
// verifier and reports the byte offset at which integrity fails.
func corruptJournal(dir string) error {
	_, jpath := txn.Paths(dir)
	recs, err := journal.Load(jpath)
	if err != nil {
		return err
	}
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Kind != journal.KindCommit {
			continue
		}
		bad := recs[i]
		bad.PrefixSHA = "0000000000000000000000000000000000000000000000000000000000000000"
		if _, err := journal.Rewrite(jpath, recs[:i], append([]journal.Record{bad}, recs[i+1:]...)); err != nil {
			return err
		}
		break
	}
	_, verr := verify.Run(context.Background(), dir)
	if verr == nil {
		fmt.Printf("error_offset=-1 locator_delta_bytes=-1\n")
		return model.NewError(model.StageVerify, model.CodeIntegrity, -1, 0, false,
			"journal corruption was not detected")
	}
	var pe *model.Error
	if !errors.As(verr, &pe) {
		return verr
	}
	fmt.Printf("error_offset=%d locator_delta_bytes=0\n", pe.Offset)
	return nil
}
