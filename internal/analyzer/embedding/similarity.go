package embedding

import (
	"container/heap"
	"encoding/csv"
	"fmt"
	"io"

	"gonum.org/v1/gonum/floats"
)

// cosineSimilarity computes cosine similarity between two float32 vectors.
// Returns 0.0 for zero-magnitude vectors (fail-safe, not NaN).
// Internally promotes to float64 via gonum/floats for numerical stability.
// For batch use, prefer cosineSimilarityBuf to avoid per-call allocations.
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0.0
	}
	af := make([]float64, len(a))
	bf := make([]float64, len(b))
	return cosineSimilarityBuf(a, b, af, bf)
}

// cosineSimilarityBuf computes cosine similarity using pre-allocated float64 buffers.
// Buffers must be at least len(a) long; they are overwritten.
func cosineSimilarityBuf(a, b []float32, af, bf []float64) float32 {
	for i, v := range a {
		af[i] = float64(v)
	}
	for i, v := range b {
		bf[i] = float64(v)
	}

	normA := floats.Norm(af[:len(a)], 2)
	normB := floats.Norm(bf[:len(b)], 2)
	if normA == 0 || normB == 0 {
		return 0.0
	}

	dot := floats.Dot(af[:len(a)], bf[:len(b)])
	return float32(dot / (normA * normB))
}

// buildSimilarityMatrix computes the full cosine similarity matrix between
// all pairs of embeddings. Returns a dense matrix with values scaled to
// [0, 100], matching the Python OllamaCrosswalker output format.
// ids provides the row/column ordering.
func buildSimilarityMatrix(embeddings map[string][]float32, ids []string) *SimilarityMatrix {
	n := len(ids)
	if n == 0 {
		return &SimilarityMatrix{IDs: ids, Values: nil}
	}

	values := make([][]float32, n)

	// Pre-allocate conversion buffers for the largest vector dimension.
	var maxDim int
	for _, vec := range embeddings {
		if len(vec) > maxDim {
			maxDim = len(vec)
		}
	}
	af := make([]float64, maxDim)
	bf := make([]float64, maxDim)

	for i := range n {
		values[i] = make([]float32, n)
		values[i][i] = 100.0
	}

	for i := range n {
		for j := i + 1; j < n; j++ {
			sim := cosineSimilarityBuf(embeddings[ids[i]], embeddings[ids[j]], af, bf)
			scaled := sim * 100.0
			values[i][j] = scaled
			values[j][i] = scaled
		}
	}

	return &SimilarityMatrix{
		IDs:    ids,
		Values: values,
	}
}

// pairHeap implements heap.Interface for SimilarityPair (min-heap by Similarity).
type pairHeap []SimilarityPair

func (h pairHeap) Len() int           { return len(h) }
func (h pairHeap) Less(i, j int) bool { return h[i].Similarity < h[j].Similarity }
func (h pairHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *pairHeap) Push(x any)        { *h = append(*h, x.(SimilarityPair)) }
func (h *pairHeap) Pop() any          { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

// topKPairs extracts the top-K most similar pairs from the matrix, excluding
// self-similarity (diagonal) and deduplicating symmetric pairs (only the
// upper triangle is considered). Returns pairs sorted by similarity descending.
func topKPairs(matrix *SimilarityMatrix, k int) []SimilarityPair {
	if k <= 0 || len(matrix.IDs) == 0 {
		return nil
	}

	h := &pairHeap{}
	heap.Init(h)

	// Scan upper triangle, maintain min-heap of size k.
	for i := range matrix.IDs {
		for j := i + 1; j < len(matrix.IDs); j++ {
			pair := SimilarityPair{
				SourceID:   matrix.IDs[i],
				TargetID:   matrix.IDs[j],
				Similarity: matrix.Values[i][j],
			}
			if h.Len() < k {
				heap.Push(h, pair)
			} else if pair.Similarity > (*h)[0].Similarity {
				(*h)[0] = pair
				heap.Fix(h, 0)
			}
		}
	}

	// Extract in descending order.
	result := make([]SimilarityPair, h.Len())
	for i := len(result) - 1; i >= 0; i-- {
		result[i] = heap.Pop(h).(SimilarityPair)
	}
	return result
}

// writeMatrixCSV writes a similarity matrix as CSV to the given writer.
// Format matches the Python OllamaCrosswalker output:
// header row with control IDs, index column with control IDs,
// values formatted to 2 decimal places.
func writeMatrixCSV(matrix *SimilarityMatrix, w io.Writer) error {
	if len(matrix.IDs) == 0 {
		return nil
	}

	cw := csv.NewWriter(w)
	defer cw.Flush()

	// Header row: empty corner cell + control IDs.
	header := make([]string, 0, len(matrix.IDs)+1)
	header = append(header, "")
	header = append(header, matrix.IDs...)
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("write CSV header: %w", err)
	}

	// Data rows: control ID + similarity values.
	row := make([]string, 0, len(matrix.IDs)+1)
	for i, id := range matrix.IDs {
		row = row[:0]
		row = append(row, id)
		for _, v := range matrix.Values[i] {
			row = append(row, fmt.Sprintf("%.2f", v))
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("write CSV row %d: %w", i, err)
		}
	}

	cw.Flush()
	return cw.Error()
}
