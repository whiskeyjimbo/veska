// Package lancedb implements the eval.VectorIndex interface using
// github.com/lancedb/lancedb-go (CGo via Rust FFI, Lance columnar format + HNSW).
//
// CGo requirement: yes — requires liblancedb_go.a and lancedb.h at build time.
// Quantization: float32 only (Lance handles its own compression internally).
//
// Save/Load semantics: the Lance database directory is the persistent form.
// Save(path) copies the directory to path (as a directory).
// Load(path) opens an existing Lance directory at path.
package lancedb

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/apache/arrow/go/v17/arrow"
	"github.com/apache/arrow/go/v17/arrow/array"
	"github.com/apache/arrow/go/v17/arrow/memory"
	"github.com/lancedb/lancedb-go/pkg/contracts"
	"github.com/lancedb/lancedb-go/pkg/lancedb"
)

const (
	tableName  = "vectors"
	colID      = "id"
	colVec     = "embedding"
	vectorDim  = 768
	batchSize  = 1000
)

// Index wraps a lancedb connection + table to implement eval.VectorIndex.
type Index struct {
	dbPath string
	conn   contracts.IConnection
	table  contracts.ITable
	n      int
	// pending holds Add calls that haven't been flushed yet.
	pendingIDs  []int64
	pendingVecs [][]float32
}

// New creates a new lancedb Index backed by a temporary in-process database at dbPath.
func New(dbPath string) (*Index, error) {
	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		return nil, fmt.Errorf("lancedb: mkdir %s: %w", dbPath, err)
	}
	conn, err := lancedb.Connect(context.Background(), dbPath, nil)
	if err != nil {
		return nil, fmt.Errorf("lancedb: connect: %w", err)
	}

	// Build the schema: id (int64) + embedding (FixedSizeList<float32>[768]).
	// Use lancedb.NewSchema(arrowSchema) directly — the SchemaBuilder's
	// AddVectorField produces a different internal type than VectorSearch expects.
	arrowSchema := arrow.NewSchema([]arrow.Field{
		{Name: colID, Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: colVec, Type: arrow.FixedSizeListOf(vectorDim, arrow.PrimitiveTypes.Float32), Nullable: false},
	}, nil)
	schema, err := lancedb.NewSchema(arrowSchema)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("lancedb: build schema: %w", err)
	}

	table, err := conn.CreateTable(context.Background(), tableName, schema)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("lancedb: create table: %w", err)
	}

	return &Index{
		dbPath: dbPath,
		conn:   conn,
		table:  table,
	}, nil
}

// Add buffers a vector. Vectors are flushed in batches of batchSize.
func (l *Index) Add(id uint64, vec []float32) error {
	l.pendingIDs = append(l.pendingIDs, int64(id))
	l.pendingVecs = append(l.pendingVecs, vec)
	if len(l.pendingIDs) >= batchSize {
		return l.flush()
	}
	return nil
}

// flush writes all pending vectors to lancedb.
func (l *Index) flush() error {
	if len(l.pendingIDs) == 0 {
		return nil
	}
	pool := memory.NewGoAllocator()
	n := len(l.pendingIDs)

	// id column
	idBldr := array.NewInt64Builder(pool)
	idBldr.AppendValues(l.pendingIDs, nil)
	idArr := idBldr.NewArray()
	defer idArr.Release()

	// Build FixedSizeList<float32>[vectorDim] for the embedding column.
	// Use the same pattern as lancedb's own tests: build flat Float32Array then
	// wrap it with NewFixedSizeListData so the inner data is contiguous.
	flat := make([]float32, n*vectorDim)
	for i, v := range l.pendingVecs {
		copy(flat[i*vectorDim:(i+1)*vectorDim], v)
	}
	f32Bldr := array.NewFloat32Builder(pool)
	f32Bldr.AppendValues(flat, nil)
	f32Arr := f32Bldr.NewArray()
	defer f32Arr.Release()

	listType := arrow.FixedSizeListOf(vectorDim, arrow.PrimitiveTypes.Float32)
	listArr := array.NewFixedSizeListData(
		array.NewData(listType, n, []*memory.Buffer{nil}, []arrow.ArrayData{f32Arr.Data()}, 0, 0),
	)
	defer listArr.Release()

	schema := arrow.NewSchema([]arrow.Field{
		{Name: colID, Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: colVec, Type: listType, Nullable: false},
	}, nil)

	rec := array.NewRecord(schema, []arrow.Array{idArr, listArr}, int64(n))
	defer rec.Release()

	if err := l.table.Add(context.Background(), rec, nil); err != nil {
		return fmt.Errorf("lancedb: flush Add: %w", err)
	}

	l.n += n
	l.pendingIDs = l.pendingIDs[:0]
	l.pendingVecs = l.pendingVecs[:0]
	return nil
}

// Search returns the k nearest ids for the query vector.
// Flushes any pending adds first.
func (l *Index) Search(query []float32, k int) ([]uint64, error) {
	if err := l.flush(); err != nil {
		return nil, err
	}
	rows, err := l.table.VectorSearch(context.Background(), colVec, query, k)
	if err != nil {
		return nil, fmt.Errorf("lancedb: vector search: %w", err)
	}
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		if v, ok := row[colID]; ok {
			switch id := v.(type) {
			case int64:
				ids = append(ids, uint64(id))
			case uint64:
				ids = append(ids, id)
			case int32:
				ids = append(ids, uint64(id))
			case float64:
				// JSON unmarshal produces float64 for all numbers.
				ids = append(ids, uint64(id))
			case float32:
				ids = append(ids, uint64(id))
			}
		}
	}
	return ids, nil
}

// Save copies the Lance database directory to path (which becomes the saved directory).
func (l *Index) Save(path string) error {
	if err := l.flush(); err != nil {
		return err
	}
	return copyDir(l.dbPath, path)
}

// Load opens an existing Lance directory at path, replacing the current connection.
func (l *Index) Load(path string) error {
	if l.table != nil {
		_ = l.table.Close()
	}
	if l.conn != nil {
		_ = l.conn.Close()
	}

	conn, err := lancedb.Connect(context.Background(), path, nil)
	if err != nil {
		return fmt.Errorf("lancedb: load connect: %w", err)
	}
	table, err := conn.OpenTable(context.Background(), tableName)
	if err != nil {
		conn.Close()
		return fmt.Errorf("lancedb: load open table: %w", err)
	}
	n, err := table.Count(context.Background())
	if err != nil {
		table.Close()
		conn.Close()
		return fmt.Errorf("lancedb: count after load: %w", err)
	}

	l.conn = conn
	l.table = table
	l.dbPath = path
	l.n = int(n)
	return nil
}

// Len returns the number of indexed vectors (flushed only).
func (l *Index) Len() int {
	return l.n
}

// Close releases lancedb resources.
func (l *Index) Close() error {
	if l.table != nil {
		_ = l.table.Close()
	}
	if l.conn != nil {
		return l.conn.Close()
	}
	return nil
}

// copyDir recursively copies src directory to dst.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
