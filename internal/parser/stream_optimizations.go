package parser

import (
	"bufio"
	"context"
	"io"
	"sync"
	"time"

	"github.com/nginxui/plugin-log-analytics/internal/cgroup"
)

// StreamParseBatches reads the stream line by line and parses it in batches
// of p.config.BatchSize, invoking fn with each parsed batch as soon as it is
// ready. Only one batch is held in memory at a time, so peak memory stays
// bounded regardless of input size. The entries passed to fn are owned by the
// callback; they are not reused by the parser.
//
// The returned ParseResult carries the counters (Processed/Succeeded/Failed)
// but a nil Entries slice.
func (p *Parser) StreamParseBatches(ctx context.Context, reader io.Reader, fn func(entries []*AccessLogEntry) error) (*ParseResult, error) {
	startTime := time.Now()
	result := &ParseResult{}

	// Use a larger buffer for better I/O performance
	const bufferSize = 64 * 1024 // 64KB buffer
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, bufferSize), p.config.MaxLineLength)

	batch := make([]string, 0, p.config.BatchSize)
	contextCheckCounter := 0
	const contextCheckFreq = 100 // Check context every 100 lines instead of every line

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		batchResult := p.ParseLinesWithContext(ctx, batch)
		batch = batch[:0]

		result.Succeeded += batchResult.Succeeded
		result.Failed += batchResult.Failed

		if fn != nil && len(batchResult.Entries) > 0 {
			return fn(batchResult.Entries)
		}
		return nil
	}

	for scanner.Scan() {
		// Reduce context checking frequency for better performance
		contextCheckCounter++
		if contextCheckCounter >= contextCheckFreq {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			default:
			}
			contextCheckCounter = 0
		}

		lineBytes := scanner.Bytes()
		if len(lineBytes) == 0 {
			continue
		}

		// Copy the line: scanner reuses its buffer between iterations
		batch = append(batch, string(lineBytes))
		result.Processed++

		if len(batch) >= p.config.BatchSize {
			if err := flush(); err != nil {
				return result, err
			}
		}
	}

	// Process remaining lines
	if err := flush(); err != nil {
		return result, err
	}

	// Check for scanner errors
	if err := scanner.Err(); err != nil {
		return result, err
	}

	result.Duration = time.Since(startTime)
	if result.Processed > 0 {
		result.ErrorRate = float64(result.Failed) / float64(result.Processed)
	}

	return result, nil
}

// StreamParse parses the whole stream and returns all entries in one slice.
// Prefer StreamParseBatches for large inputs: this variant accumulates every
// entry in memory and is only appropriate for bounded inputs such as
// incremental tails.
func (p *Parser) StreamParse(ctx context.Context, reader io.Reader) (*ParseResult, error) {
	entries := make([]*AccessLogEntry, 0, 10000)
	result, err := p.StreamParseBatches(ctx, reader, func(batch []*AccessLogEntry) error {
		entries = append(entries, batch...)
		return nil
	})
	result.Entries = entries
	return result, err
}

// ChunkedParseStream is a deprecated alias of StreamParse kept for
// compatibility; the chunkSize parameter is ignored. The single streaming
// implementation already reads with a bounded buffer.
func (p *Parser) ChunkedParseStream(ctx context.Context, reader io.Reader, _ int) (*ParseResult, error) {
	return p.StreamParse(ctx, reader)
}

// MemoryEfficientParseStream is a deprecated alias of StreamParse kept for
// compatibility. Use StreamParseBatches for genuinely bounded-memory parsing.
func (p *Parser) MemoryEfficientParseStream(ctx context.Context, reader io.Reader) (*ParseResult, error) {
	return p.StreamParse(ctx, reader)
}

// ParseLinesOrdered parses the lines and returns one slot per line, in the
// order of the input. A slot is nil when its line could not be parsed, so a
// caller can map every entry back to the line, and to the file offset, it came
// from. Large inputs are parsed by several workers over contiguous ranges.
func (p *Parser) ParseLinesOrdered(ctx context.Context, lines []string) []*AccessLogEntry {
	entries := make([]*AccessLogEntry, len(lines))
	if len(lines) == 0 {
		return entries
	}

	workers := p.config.WorkerCount
	if workers <= 0 {
		workers = cgroup.AvailableCPUs()
	}
	if len(lines) < p.config.BatchSize || workers <= 1 {
		p.parseRange(ctx, lines, entries)
		return entries
	}
	if workers > len(lines)/10+1 {
		workers = len(lines)/10 + 1
	}

	chunk := (len(lines) + workers - 1) / workers
	var wg sync.WaitGroup
	for start := 0; start < len(lines); start += chunk {
		end := min(start+chunk, len(lines))
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			p.parseRange(ctx, lines[lo:hi], entries[lo:hi])
		}(start, end)
	}
	wg.Wait()
	return entries
}

// parseRange parses lines into out, which has the same length.
func (p *Parser) parseRange(ctx context.Context, lines []string, out []*AccessLogEntry) {
	for i, line := range lines {
		if i%256 == 0 && ctx.Err() != nil {
			return
		}
		if entry, err := p.ParseLine(line); err == nil {
			out[i] = entry
		}
	}
}
