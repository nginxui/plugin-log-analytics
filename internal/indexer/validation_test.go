package indexer

import (
	"testing"
)

func TestThroughputOptimizations(t *testing.T) {
	config := DefaultIndexerConfig()

	t.Run("BoundedBatchSizes", func(t *testing.T) {
		// Batch size times concurrency sets the peak memory, so it stays small.
		if config.BatchSize < 1 || config.BatchSize > largeBatchSize {
			t.Errorf("Expected batch size in [1, %d], got %d", largeBatchSize, config.BatchSize)
		}
		t.Logf("Batch size: %d", config.BatchSize)
	})
}

func TestParserBatchSizeOptimization(t *testing.T) {
	t.Run("ParserConfigOptimized", func(t *testing.T) {
		// Since parser.go has a global init, we need to check if it's properly configured
		// We'll test this by verifying the default config is optimized
		config := DefaultIndexerConfig()

		// IndexDocuments is synchronous, so the queue only needs a small
		// multiple of the worker count. Large queues retain entire batches.
		expectedMaxQueue := config.WorkerCount * 4
		if config.MaxQueueSize > expectedMaxQueue {
			t.Errorf("Expected queue size <= %d, got %d", expectedMaxQueue, config.MaxQueueSize)
		}

		t.Logf("Parser configuration: BatchSize=%d, QueueSize=%d",
			config.BatchSize, config.MaxQueueSize)
	})
}

// Benchmark to verify performance characteristics
func BenchmarkBatchSizeCalculation(b *testing.B) {
	config := DefaultIndexerConfig()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = config.BatchSize * 2
	}
}
