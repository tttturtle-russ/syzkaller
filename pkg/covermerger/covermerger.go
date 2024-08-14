// Copyright 2024 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package covermerger

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/google/syzkaller/pkg/log"
	"golang.org/x/exp/maps"
	"golang.org/x/sync/errgroup"
)

const (
	KeyKernelRepo   = "kernel_repo"
	KeyKernelBranch = "kernel_branch"
	KeyKernelCommit = "kernel_commit"
	KeyFilePath     = "file_path"
	KeyStartLine    = "sl"
	KeyHitCount     = "hit_count"
)

type FileRecord struct {
	FilePath string
	RepoBranchCommit
	StartLine int
	HitCount  int
}

type RepoBranchCommit struct {
	Repo   string
	Branch string
	Commit string
}

type MergeResult struct {
	HitCounts  map[int]int
	FileExists bool
}

type FileCoverageMerger interface {
	Add(record *FileRecord)
	Result() *MergeResult
}

func batchFileData(c *Config, targetFilePath string, records []*FileRecord) (*MergeResult, error) {
	log.Logf(1, "processing %d records for %s", len(records), targetFilePath)
	repoBranchCommitsMap := make(map[RepoBranchCommit]bool)
	for _, record := range records {
		repoBranchCommitsMap[record.RepoBranchCommit] = true
	}
	repoBranchCommitsMap[c.Base] = true
	repoBranchCommits := maps.Keys(repoBranchCommitsMap)
	fvs, err := c.FileVersProvider.GetFileVersions(c, targetFilePath, repoBranchCommits)
	if err != nil {
		return nil, fmt.Errorf("failed to getFileVersions: %w", err)
	}
	merger := makeFileLineCoverMerger(fvs, c.Base)
	for _, record := range records {
		merger.Add(record)
	}
	return merger.Result(), nil
}

func makeRecord(fields, schema []string) *FileRecord {
	if len(fields) != len(schema) {
		panic("fields size and schema size are not equal")
	}
	record := &FileRecord{}
	for i, val := range fields {
		key := schema[i]
		switch key {
		case KeyFilePath:
			record.FilePath = val
		case KeyKernelRepo:
			record.Repo = val
		case KeyKernelBranch:
			record.Branch = val
		case KeyKernelCommit:
			record.Commit = val
		case KeyStartLine:
			record.StartLine = readIntField(key, val)
		case KeyHitCount:
			record.HitCount = readIntField(key, val)
		}
	}
	return record
}

func readIntField(field, val string) int {
	res, err := strconv.Atoi(val)
	if err != nil {
		panic(fmt.Errorf("failed to Atoi(%s) %s: %w", val, field, err))
	}
	return res
}

type Config struct {
	Jobs             int
	Workdir          string
	skipRepoClone    bool
	Base             RepoBranchCommit
	FileVersProvider FileVersProvider
}

func isSchema(fields, schema []string) bool {
	if len(fields) != len(schema) {
		return false
	}
	for i := 0; i < len(fields); i++ {
		if fields[i] != schema[i] {
			return false
		}
	}
	return true
}

func MergeCSVData(config *Config, reader io.Reader) (map[string]*MergeResult, error) {
	var schema []string
	csvReader := csv.NewReader(reader)
	if fields, err := csvReader.Read(); err != nil {
		return nil, fmt.Errorf("failed to read schema: %w", err)
	} else {
		schema = fields
	}
	errStdinReadChan := make(chan error, 1)
	recordsChan := make(chan *FileRecord)
	go func() {
		defer close(recordsChan)
		for {
			fields, err := csvReader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				errStdinReadChan <- fmt.Errorf("failed to read CSV line: %w", err)
				return
			}
			if isSchema(fields, schema) {
				// The input may be the merged CVS files with multiple schemas.
				continue
			}
			recordsChan <- makeRecord(fields, schema)
		}
		errStdinReadChan <- nil
	}()
	mergeResult, errMerging := mergeChanData(config, recordsChan)
	errStdinRead := <-errStdinReadChan
	if errMerging != nil || errStdinRead != nil {
		return nil, fmt.Errorf("errors merging stdin data:\nmerger err: %w\nstdin reader err: %w",
			errMerging, errStdinRead)
	}
	return mergeResult, nil
}

type FileRecords struct {
	fileName string
	records  []*FileRecord
}

func mergeChanData(c *Config, recordChan <-chan *FileRecord) (map[string]*MergeResult, error) {
	g, ctx := errgroup.WithContext(context.Background())
	frecordChan := groupFileRecords(recordChan, ctx)
	stat := make(map[string]*MergeResult)
	var mu sync.Mutex
	for i := 0; i < c.Jobs; i++ {
		g.Go(func() error {
			for frecord := range frecordChan {
				if mr, err := batchFileData(c, frecord.fileName, frecord.records); err != nil {
					return fmt.Errorf("failed to batchFileData(%s): %w", frecord.fileName, err)
				} else {
					mu.Lock()
					if _, exist := stat[frecord.fileName]; exist {
						mu.Unlock()
						return fmt.Errorf("file %s was already processed", frecord.fileName)
					}
					stat[frecord.fileName] = mr
					mu.Unlock()
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return stat, nil
}

func groupFileRecords(recordChan <-chan *FileRecord, ctx context.Context) chan FileRecords {
	frecordChan := make(chan FileRecords)
	go func() {
		defer close(frecordChan)
		targetFile := ""
		var records []*FileRecord
		for record := range recordChan {
			select {
			case <-ctx.Done():
				return
			default:
			}
			curTargetFile := record.FilePath
			if targetFile == "" {
				targetFile = curTargetFile
			}
			if curTargetFile != targetFile {
				frecordChan <- FileRecords{
					fileName: targetFile,
					records:  records,
				}
				records = nil
				targetFile = curTargetFile
			}
			records = append(records, record)
		}
		frecordChan <- FileRecords{
			fileName: targetFile,
			records:  records,
		}
	}()
	return frecordChan
}
