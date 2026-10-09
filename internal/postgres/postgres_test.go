//go:build integration

// Интеграционные тесты на настоящем PostgreSQL.
//
// Первая строка «//go:build integration» — build-тег: файл компилируется
// только с флагом -tags integration, поэтому обычный «go test ./...» не
// требует базы данных.
//
// Тесты очищают таблицы (TRUNCATE), поэтому направляйте их на ОТДЕЛЬНУЮ
// базу, а не на ту, с которой работает приложение:
//
//	TEST_DATABASE_URL=postgres://sitewatch:sitewatch@localhost:5432/sitewatch_test?sslmode=disable \
//	  go test -tags integration ./internal/postgres/
package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"sitewatch/internal/site"
)

// newTestRepo подключается к тестовой базе, применяет миграции, очищает
// таблицы и возвращает репозиторий. Если TEST_DATABASE_URL не задан,
// тест пропускается (t.Skip), а не падает.
func newTestRepo(t *testing.T) *Repository {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	// t.Cleanup регистрирует функцию, которая выполнится после окончания теста.
	t.Cleanup(pool.Close)

	if _, err = Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// RESTART IDENTITY обнуляет и счётчики id, чтобы каждый запуск начинался
	// с одинакового состояния.
	if _, err = pool.Exec(ctx, `TRUNCATE sites, checks RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}
	return NewRepository(pool)
}

// TestRepository — сценарий от создания сайта до его удаления, который
// проверяет все методы репозитория на реальных SQL-запросах.
func TestRepository(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	// Создание: база должна заполнить id и created_at.
	created, err := repo.CreateSite(ctx, site.Site{Name: "Go", URL: "https://go.dev"})
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if created.ID == 0 || created.CreatedAt.IsZero() {
		t.Fatalf("id and created_at must be filled: %+v", created)
	}

	// Повторный URL → доменная ошибка ErrAlreadyExists (из кода PostgreSQL 23505).
	if _, err = repo.CreateSite(ctx, site.Site{Name: "dup", URL: "https://go.dev"}); !errors.Is(err, site.ErrAlreadyExists) {
		t.Fatalf("duplicate: err = %v, want ErrAlreadyExists", err)
	}

	// Пока проверок нет, LastCheck должен быть nil.
	sites, err := repo.ListSites(ctx)
	if err != nil || len(sites) != 1 || sites[0].LastCheck != nil {
		t.Fatalf("ListSites before checks = %+v, %v", sites, err)
	}

	// Время усечено до миллисекунд и переведено в UTC, чтобы значения
	// совпадали после сохранения в базе (точность и часовой пояс).
	now := time.Now().UTC().Truncate(time.Millisecond)
	err = repo.SaveChecks(ctx, []site.Check{
		{SiteID: created.ID, StatusCode: 500, LatencyMS: 10, CheckedAt: now.Add(-time.Minute)},
		{SiteID: created.ID, StatusCode: 200, LatencyMS: 20, CheckedAt: now},
		{SiteID: 999_999, StatusCode: 200, CheckedAt: now}, // несуществующий сайт — должен быть пропущен
	})
	if err != nil {
		t.Fatalf("SaveChecks: %v", err)
	}

	// В списке сайтов должна оказаться самая свежая проверка (200, а не 500).
	sites, err = repo.ListSites(ctx)
	if err != nil || sites[0].LastCheck == nil || sites[0].LastCheck.StatusCode != 200 {
		t.Fatalf("ListSites must return the latest check: %+v, %v", sites, err)
	}

	// В истории — ровно 2 проверки: третья (чужой сайт) отброшена.
	checks, err := repo.ListChecks(ctx, created.ID, 10)
	if err != nil || len(checks) != 2 {
		t.Fatalf("ListChecks = %+v, %v", checks, err)
	}
	if _, err = repo.ListChecks(ctx, 999_999, 10); !errors.Is(err, site.ErrNotFound) {
		t.Fatalf("ListChecks of unknown site: err = %v, want ErrNotFound", err)
	}

	// Удаление, затем повторное удаление того же сайта → ErrNotFound.
	if err = repo.DeleteSite(ctx, created.ID); err != nil {
		t.Fatalf("DeleteSite: %v", err)
	}
	if err = repo.DeleteSite(ctx, created.ID); !errors.Is(err, site.ErrNotFound) {
		t.Fatalf("second DeleteSite: err = %v, want ErrNotFound", err)
	}
}
