package doctor_test

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/doctor"
)

type fakeRefs struct {
	counts map[string]int
	err    error
}

func (f *fakeRefs) CountByState(_ context.Context) (map[string]int, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.counts, nil
}

func TestCheckEmbedQueueHealth_Healthy(t *testing.T) {
	t.Parallel()
	got, err := doctor.CheckEmbedQueueHealth(context.Background(), &fakeRefs{
		counts: map[string]int{"pending": 10, "ready": 100, "failed": 0},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Status != "healthy" {
		t.Errorf("status: want healthy, got %q (report=%+v)", got.Status, got)
	}
	if got.Pending != 10 || got.Ready != 100 || got.Failed != 0 {
		t.Errorf("counts: %+v", got)
	}
}

func TestCheckEmbedQueueHealth_BrokenOnFailed(t *testing.T) {
	t.Parallel()
	got, _ := doctor.CheckEmbedQueueHealth(context.Background(), &fakeRefs{
		counts: map[string]int{"pending": 0, "ready": 0, "failed": 1},
	})
	if got.Status != "broken" {
		t.Errorf("status: want broken, got %q", got.Status)
	}
}

func TestCheckEmbedQueueHealth_DegradedOnBacklog(t *testing.T) {
	t.Parallel()
	got, _ := doctor.CheckEmbedQueueHealth(context.Background(), &fakeRefs{
		counts: map[string]int{"pending": 2000, "ready": 0, "failed": 0},
	})
	if got.Status != "degraded" {
		t.Errorf("status: want degraded, got %q", got.Status)
	}
}

func TestCheckEmbedQueueHealth_BrokenBeatsDegraded(t *testing.T) {
	t.Parallel()
	got, _ := doctor.CheckEmbedQueueHealth(context.Background(), &fakeRefs{
		counts: map[string]int{"pending": 2000, "ready": 0, "failed": 1},
	})
	if got.Status != "broken" {
		t.Errorf("status: want broken (precedence), got %q", got.Status)
	}
}

func TestCheckEmbedQueueHealth_NilRefs(t *testing.T) {
	t.Parallel()
	got, _ := doctor.CheckEmbedQueueHealth(context.Background(), nil)
	if got.Status != "broken" {
		t.Errorf("status: want broken on nil refs, got %q", got.Status)
	}
}

func TestCheckEmbedQueueHealth_QueryError(t *testing.T) {
	t.Parallel()
	got, _ := doctor.CheckEmbedQueueHealth(context.Background(), &fakeRefs{
		err: errors.New("db gone"),
	})
	if got.Status != "broken" {
		t.Errorf("status: want broken on query error, got %q", got.Status)
	}
}
