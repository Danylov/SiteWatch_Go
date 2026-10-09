package worker

// Тесты пула воркеров. Запускайте их и с флагом -race: детектор гонок
// проверит, что горутины не обращаются к общим данным без синхронизации.

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// TestRun_ProcessesAllJobs проверяет, что каждая задача обработана ровно
// один раз и все результаты возвращены.
func TestRun_ProcessesAllJobs(t *testing.T) {
	jobs := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	got := Run(context.Background(), 3, jobs, func(_ context.Context, n int) int {
		return n * n
	})

	// Порядок результатов не гарантирован, поэтому перед сравнением сортируем.
	slices.Sort(got)
	want := []int{1, 4, 9, 16, 25, 36, 49, 64, 81, 100}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestRun_RespectsWorkerLimit проверяет, что одновременно выполняется
// не больше workers задач.
func TestRun_RespectsWorkerLimit(t *testing.T) {
	const workers = 3
	// atomic.Int32 — счётчик, который безопасно менять из многих горутин
	// без мьютекса. inFlight — сколько задач выполняется прямо сейчас,
	// maxInFlight — максимум, замеченный за всё время.
	var inFlight, maxInFlight atomic.Int32

	jobs := make([]int, 30)
	Run(context.Background(), workers, jobs, func(_ context.Context, _ int) struct{} {
		cur := inFlight.Add(1) // задача началась
		defer inFlight.Add(-1) // задача закончилась
		// Lock-free обновление максимума: читаем текущий максимум и пытаемся
		// атомарно заменить его на cur. CompareAndSwap(old, cur) сработает,
		// только если значение всё ещё равно old; если другая горутина успела
		// его изменить — повторяем попытку.
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond) // «работа», чтобы задачи пересекались по времени
		return struct{}{}                // struct{} — пустой тип: результат не нужен
	})

	if m := maxInFlight.Load(); m > workers {
		t.Fatalf("max concurrent jobs = %d, want <= %d", m, workers)
	}
}

// TestRun_StopsOnCancel проверяет, что после отмены контекста пул перестаёт
// раздавать задачи и завершается раньше, чем обработает их все.
func TestRun_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var processed atomic.Int32

	jobs := make([]int, 1000)
	results := Run(ctx, 2, jobs, func(ctx context.Context, _ int) int {
		if processed.Add(1) == 10 {
			cancel() // на 10-й задаче «нажимаем отмену»
		}
		// Имитация работы, которую можно прервать отменой контекста.
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
		}
		return 0
	})

	if len(results) >= len(jobs) {
		t.Fatalf("expected the pool to stop early, but all %d jobs were processed", len(results))
	}
}

// TestRun_EmptyJobs проверяет крайний случай: пустой список задач даёт
// пустой результат и не приводит к зависанию.
func TestRun_EmptyJobs(t *testing.T) {
	got := Run(context.Background(), 4, []int(nil), func(_ context.Context, n int) int { return n })
	if len(got) != 0 {
		t.Fatalf("expected no results, got %v", got)
	}
}
