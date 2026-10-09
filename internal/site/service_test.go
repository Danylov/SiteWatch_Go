package site

// Тесты бизнес-логики. Вместо базы данных и HTTP используются моки
// (MockRepository, MockChecker из mocks_test.go): тест заранее сообщает моку,
// какие вызовы ожидаются и что вернуть. Если сервис вызовет то, чего не
// ждали, или не вызовет ожидаемое, gomock уронит тест автоматически.
//
// Полезное из gomock:
//   - gomock.Any()     — подходит любой аргумент;
//   - gomock.Len(n)    — срез длины n;
//   - .Return(...)     — что вернуть;
//   - .DoAndReturn(fn) — выполнить свою функцию (например, ждать на канале);
//   - .AnyTimes()      — вызов может случиться сколько угодно раз.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"
)

// newTestService создаёт сервис с моками: 3 воркера, таймаут прогона 1 с.
// gomock.NewController(t) сам проверит ожидания в конце теста (через t.Cleanup).
func newTestService(t *testing.T) (*Service, *MockRepository, *MockChecker) {
	// t.Helper(): при падении Go покажет строку в вызывающем тесте,
	// а не внутри этой вспомогательной функции.
	t.Helper()
	ctrl := gomock.NewController(t)
	repo := NewMockRepository(ctrl)
	checker := NewMockChecker(ctrl)
	return NewService(repo, checker, 3, time.Second), repo, checker
}

// TestCreateSite_NormalizesInput проверяет нормализацию ввода: пробелы по
// краям и завершающий «/» должны быть убраны до обращения к репозиторию.
func TestCreateSite_NormalizesInput(t *testing.T) {
	svc, repo, _ := newTestService(t)
	ctx := context.Background()

	// Ожидание с ТОЧНЫМИ аргументами: если сервис передаст в репозиторий
	// что-то другое (например, "  Go "), мок не совпадёт, и тест упадёт.
	repo.EXPECT().
		CreateSite(ctx, Site{Name: "Go", URL: "https://go.dev"}).
		Return(Site{ID: 1, Name: "Go", URL: "https://go.dev"}, nil)

	got, err := svc.CreateSite(ctx, CreateInput{Name: "  Go ", URL: " https://go.dev/ "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != 1 {
		t.Errorf("ID = %d, want 1", got.ID)
	}
}

// TestCreateSite_PreservesDomainError проверяет, что ошибка, обёрнутая
// сервисом через %w, по-прежнему распознаётся errors.Is.
func TestCreateSite_PreservesDomainError(t *testing.T) {
	svc, repo, _ := newTestService(t)

	repo.EXPECT().CreateSite(gomock.Any(), gomock.Any()).Return(Site{}, ErrAlreadyExists)

	_, err := svc.CreateSite(context.Background(), CreateInput{Name: "Go", URL: "https://go.dev"})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
}

// TestListChecks_Limit — табличный тест правила лимита: 0 → значение по
// умолчанию, обычное значение передаётся как есть, слишком большое обрезается.
func TestListChecks_Limit(t *testing.T) {
	tests := []struct {
		name      string
		limit     int // что передал клиент
		wantLimit int // что должно дойти до репозитория
	}{
		{"default when zero", 0, defaultChecksLimit},
		{"passed through", 10, 10},
		{"capped", 1000, maxChecksLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo, _ := newTestService(t)
			repo.EXPECT().ListChecks(gomock.Any(), int64(7), tt.wantLimit).Return(nil, nil)

			if _, err := svc.ListChecks(context.Background(), 7, tt.limit); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestRunChecks_ChecksAllSitesAndSavesResults — основной сценарий прогона:
// три сайта (ответ 200, ответ 500, сетевая ошибка) проверены, результаты
// сохранены, итог посчитан верно и запомнен как последний прогон.
func TestRunChecks_ChecksAllSitesAndSavesResults(t *testing.T) {
	svc, repo, checker := newTestService(t)

	sites := []Site{
		{ID: 1, URL: "https://ok.example"},
		{ID: 2, URL: "https://broken.example"},
		{ID: 3, URL: "https://down.example"},
	}
	repo.EXPECT().ListSites(gomock.Any()).Return(sites, nil)

	checker.EXPECT().Check(gomock.Any(), "https://ok.example").Return(200, 30*time.Millisecond, nil)
	checker.EXPECT().Check(gomock.Any(), "https://broken.example").Return(500, 10*time.Millisecond, nil)
	checker.EXPECT().Check(gomock.Any(), "https://down.example").Return(0, 0*time.Millisecond, errors.New("connection refused"))

	// DoAndReturn перехватывает срез, который сервис передал в SaveChecks,
	// чтобы потом проверить его содержимое.
	var saved []Check
	repo.EXPECT().SaveChecks(gomock.Any(), gomock.Len(3)).
		DoAndReturn(func(_ context.Context, checks []Check) error {
			saved = checks
			return nil
		})

	summary, err := svc.RunChecks(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 500 и сетевая ошибка — оба DOWN.
	if summary.Total != 3 || summary.Up != 1 || summary.Down != 2 {
		t.Errorf("summary = %+v, want total=3 up=1 down=2", summary)
	}

	// Текст сетевой ошибки должен сохраниться в результате проверки.
	for _, c := range saved {
		if c.SiteID == 3 && c.Error != "connection refused" {
			t.Errorf("error of site 3 = %q, want %q", c.Error, "connection refused")
		}
	}

	last, ok := svc.LastRun()
	if !ok || last != summary { // структуры без срезов/map можно сравнивать через ==
		t.Errorf("LastRun() = %+v, %v; want %+v, true", last, ok, summary)
	}
}

// TestRunChecks_NoSites: если сайтов нет, сохранять нечего.
func TestRunChecks_NoSites(t *testing.T) {
	svc, repo, _ := newTestService(t)
	repo.EXPECT().ListSites(gomock.Any()).Return(nil, nil)
	// SaveChecks вызываться не должен: ожидания для него нет, и gomock
	// уронит тест, если вызов всё же случится.

	summary, err := svc.RunChecks(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.Total != 0 {
		t.Errorf("Total = %d, want 0", summary.Total)
	}
}

// TestRunChecks_RepositoryError: ошибка загрузки сайтов возвращается наверх
// (в обёртке), а неудачный прогон не записывается как последний.
func TestRunChecks_RepositoryError(t *testing.T) {
	svc, repo, _ := newTestService(t)
	dbErr := errors.New("connection lost")
	repo.EXPECT().ListSites(gomock.Any()).Return(nil, dbErr)

	if _, err := svc.RunChecks(context.Background()); !errors.Is(err, dbErr) {
		t.Fatalf("err = %v, want wrapped %v", err, dbErr)
	}
	if _, ok := svc.LastRun(); ok {
		t.Error("failed run must not be stored as the last run")
	}
}

// TestRunChecks_RejectsConcurrentRun: второй прогон, запущенный, пока идёт
// первый, должен получить ErrCheckInProgress.
// Запускайте с -race: флаг running разделяется между горутинами.
//
// Приём: чтобы детерминированно проверить конкурентное поведение, нельзя
// полагаться на time.Sleep. Каналы started/release позволяют точно
// «остановить» первую горутину в нужной точке, выполнить проверку и затем
// «отпустить» её.
func TestRunChecks_RejectsConcurrentRun(t *testing.T) {
	svc, repo, checker := newTestService(t)

	started := make(chan struct{}) // закрывается, когда первый прогон дошёл до проверки
	release := make(chan struct{}) // закрываем, чтобы первый прогон продолжился

	repo.EXPECT().ListSites(gomock.Any()).Return([]Site{{ID: 1, URL: "https://slow.example"}}, nil)
	checker.EXPECT().Check(gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, string) (int, time.Duration, error) {
			close(started) // сигнал: первый прогон точно идёт
			<-release      // ждём разрешения продолжить
			return 200, time.Millisecond, nil
		})
	repo.EXPECT().SaveChecks(gomock.Any(), gomock.Any()).Return(nil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// В горутине используется t.Errorf, а не t.Fatalf: Fatal можно
		// вызывать только из горутины самого теста.
		if _, err := svc.RunChecks(context.Background()); err != nil {
			t.Errorf("first run: unexpected error: %v", err)
		}
	}()

	<-started // дожидаемся, пока первый прогон «застрянет» внутри Check
	if _, err := svc.RunChecks(context.Background()); !errors.Is(err, ErrCheckInProgress) {
		t.Errorf("second run: err = %v, want ErrCheckInProgress", err)
	}

	close(release) // отпускаем первый прогон
	wg.Wait()      // и дожидаемся его завершения до конца теста
}

// TestRunChecks_TimeoutStillSavesFinishedChecks: при истечении таймаута
// прогона уже полученные результаты не теряются, а зависшая проверка
// сохраняется как DOWN.
func TestRunChecks_TimeoutStillSavesFinishedChecks(t *testing.T) {
	// Здесь свой сервис: 1 воркер и короткий таймаут прогона 50 мс.
	ctrl := gomock.NewController(t)
	repo := NewMockRepository(ctrl)
	checker := NewMockChecker(ctrl)
	svc := NewService(repo, checker, 1, 50*time.Millisecond)

	repo.EXPECT().ListSites(gomock.Any()).Return([]Site{
		{ID: 1, URL: "https://fast.example"},
		{ID: 2, URL: "https://hangs.example"},
	}, nil)
	checker.EXPECT().Check(gomock.Any(), "https://fast.example").Return(200, time.Millisecond, nil)
	checker.EXPECT().Check(gomock.Any(), "https://hangs.example").
		DoAndReturn(func(ctx context.Context, _ string) (int, time.Duration, error) {
			<-ctx.Done() // таймаут прогона должен прервать «зависшую» проверку
			return 0, 50 * time.Millisecond, ctx.Err()
		})
	repo.EXPECT().SaveChecks(gomock.Any(), gomock.Len(2)).Return(nil)

	summary, err := svc.RunChecks(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.Up != 1 || summary.Down != 1 {
		t.Errorf("summary = %+v, want up=1 down=1", summary)
	}
}

// TestRunScheduler_StopsOnCancel: планировщик должен завершиться вскоре
// после отмены контекста.
func TestRunScheduler_StopsOnCancel(t *testing.T) {
	svc, repo, _ := newTestService(t)
	// За время теста тикер сработает несколько раз — точное число неизвестно.
	repo.EXPECT().ListSites(gomock.Any()).Return(nil, nil).AnyTimes()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}) // закроется, когда RunScheduler вернётся
	go func() {
		svc.RunScheduler(ctx, 5*time.Millisecond, discardLogger())
		close(done)
	}()

	time.Sleep(20 * time.Millisecond) // даём планировщику поработать
	cancel()

	// Ждём завершения, но не дольше секунды — иначе тест завис бы навсегда.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after context cancellation")
	}
}
