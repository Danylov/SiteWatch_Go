package site

// Директива для «go generate ./...»: запускает mockgen, который читает
// интерфейсы из этого файла и генерирует моки в mocks_test.go.
// Обычная сборка (go build) такие комментарии игнорирует.
//go:generate go tool mockgen -source=service.go -destination=mocks_test.go -package=site

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"sitewatch/internal/worker"
)

// Repository — хранилище, которое нужно сервису.
//
// Интерфейс объявлен на стороне ПОТРЕБИТЕЛЯ (здесь, в пакете site), а не
// рядом с реализацией (postgres). В Go интерфейсы реализуются неявно: любой
// тип с такими методами подходит. Поэтому в рабочем коде сюда передаётся
// *postgres.Repository, а в тестах — сгенерированный MockRepository.
type Repository interface {
	// CreateSite сохраняет сайт и возвращает его с заполненными ID и CreatedAt.
	// Если URL уже есть — ErrAlreadyExists.
	CreateSite(ctx context.Context, s Site) (Site, error)
	// ListSites возвращает все сайты, у каждого — последнюю проверку.
	ListSites(ctx context.Context) ([]Site, error)
	// DeleteSite удаляет сайт; если его нет — ErrNotFound.
	DeleteSite(ctx context.Context, id int64) error
	// SaveChecks сохраняет пачку результатов проверок.
	SaveChecks(ctx context.Context, checks []Check) error
	// ListChecks возвращает последние limit проверок сайта, новые первыми.
	// Если сайта нет — ErrNotFound.
	ListChecks(ctx context.Context, siteID int64, limit int) ([]Check, error)
}

// Checker проверяет URL и возвращает HTTP-код ответа и задержку.
// Реализация — *checker.Checker, в тестах — MockChecker.
// Имена результатов (status, latency, err) здесь служат документацией.
type Checker interface {
	Check(ctx context.Context, url string) (status int, latency time.Duration, err error)
}

const (
	defaultChecksLimit = 20  // сколько проверок отдавать, если limit не указан
	maxChecksLimit     = 100 // верхняя граница, чтобы не выгружать всю историю
)

// Service реализует все сценарии программы: работу со списком сайтов,
// прогон проверок и фоновый планировщик.
//
// Service всегда используется через указатель (*Service): в нём есть
// sync.Mutex, а мьютекс нельзя копировать — копия была бы другим замком,
// и защита данных потерялась бы (go vet предупреждает о таком копировании).
type Service struct {
	repo       Repository
	checker    Checker
	workers    int
	runTimeout time.Duration
	now        func() time.Time // источник текущего времени; в тестах можно подменить

	mu      sync.Mutex  // защищает поля ниже: к ним обращаются разные горутины
	running bool        // идёт ли сейчас прогон
	lastRun *RunSummary // итог последнего прогона; nil — прогонов ещё не было
}

// NewService создаёт сервис с указанными зависимостями и настройками.
//
// В Go нет конструкторов как в Java — принято писать функцию NewXxx.
// Поля mu, running и lastRun не инициализируются: их «нулевые значения»
// (открытый мьютекс, false, nil) — как раз то, что нужно.
func NewService(repo Repository, checker Checker, workers int, runTimeout time.Duration) *Service {
	return &Service{
		repo:       repo,
		checker:    checker,
		workers:    workers,
		runTimeout: runTimeout,
		now:        time.Now,
	}
}

// CreateSite добавляет новый сайт, предварительно нормализовав ввод.
func (s *Service) CreateSite(ctx context.Context, in CreateInput) (Site, error) {
	// Нормализация: убираем пробелы по краям и завершающие «/» в URL.
	// Тогда "https://go.dev/" и "https://go.dev" считаются одним сайтом,
	// и ограничение уникальности URL в базе срабатывает правильно.
	site := Site{
		Name: strings.TrimSpace(in.Name),
		URL:  strings.TrimRight(strings.TrimSpace(in.URL), "/"),
	}
	created, err := s.repo.CreateSite(ctx, site)
	if err != nil {
		// Обёртка через %w сохраняет исходную ошибку внутри: наверху
		// errors.Is(err, ErrAlreadyExists) по-прежнему сработает.
		return Site{}, fmt.Errorf("create site: %w", err)
	}
	return created, nil
}

// ListSites возвращает все сайты вместе с их последними проверками.
func (s *Service) ListSites(ctx context.Context) ([]Site, error) {
	sites, err := s.repo.ListSites(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}
	return sites, nil
}

// DeleteSite удаляет сайт по id. История его проверок удаляется базой
// автоматически (ON DELETE CASCADE в миграции).
func (s *Service) DeleteSite(ctx context.Context, id int64) error {
	if err := s.repo.DeleteSite(ctx, id); err != nil {
		return fmt.Errorf("delete site %d: %w", id, err)
	}
	return nil
}

// ListChecks возвращает историю проверок сайта, применяя правило лимита:
// limit <= 0 означает «по умолчанию» (20), больше 100 не отдаём.
func (s *Service) ListChecks(ctx context.Context, siteID int64, limit int) ([]Check, error) {
	if limit <= 0 {
		limit = defaultChecksLimit
	}
	limit = min(limit, maxChecksLimit) // min — встроенная функция (Go 1.21+)

	checks, err := s.repo.ListChecks(ctx, siteID, limit)
	if err != nil {
		return nil, fmt.Errorf("list checks of site %d: %w", siteID, err)
	}
	return checks, nil
}

// RunChecks параллельно проверяет все сайты через пул воркеров и сохраняет
// результаты. Одновременно может идти только один прогон: повторный вызов
// во время работы сразу получает ErrCheckInProgress.
func (s *Service) RunChecks(ctx context.Context) (RunSummary, error) {
	// Под мьютексом только проверяем и ставим флаг — это мгновенно.
	// Сам долгий прогон (сеть, БД) идёт БЕЗ блокировки, иначе, например,
	// LastRun() ждал бы окончания прогона.
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return RunSummary{}, ErrCheckInProgress
	}
	s.running = true
	s.mu.Unlock()

	// Флаг сбрасывается при ЛЮБОМ выходе из функции, в том числе по ошибке.
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	start := s.now()

	sites, err := s.repo.ListSites(ctx)
	if err != nil {
		return RunSummary{}, fmt.Errorf("list sites: %w", err)
	}

	// Общий лимит времени на весь прогон. Каждая проверка дополнительно
	// ограничена своим таймаутом внутри checker — сработает то, что раньше.
	// cancel освобождает таймер контекста; вызывать его нужно всегда.
	runCtx, cancel := context.WithTimeout(ctx, s.runTimeout)
	defer cancel()

	// Site передаётся в функцию по значению: у каждого воркера своя копия,
	// общих изменяемых данных нет — значит, нет и гонок.
	// Замыкание выполняется в горутинах-воркерах, поэтому не трогает внешнюю
	// err (все воркеры писали бы в одну переменную — гонка данных), а его
	// переменные названы по-своему: jobCtx, checkErr.
	checks := worker.Run(runCtx, s.workers, sites, func(jobCtx context.Context, site Site) Check {
		status, latency, checkErr := s.checker.Check(jobCtx, site.URL)
		c := Check{
			SiteID:     site.ID,
			StatusCode: status,
			LatencyMS:  latency.Milliseconds(),
			CheckedAt:  s.now(),
		}
		if checkErr != nil {
			// Ошибка проверки — не ошибка прогона: сайт просто недоступен,
			// это нормальный результат, его тоже нужно сохранить.
			c.Error = checkErr.Error()
		}
		return c
	})

	// Сохраняем с родительским ctx, а не с runCtx: даже если прогон упёрся
	// в таймаут, уже полученные результаты стоит записать.
	if len(checks) > 0 {
		if err = s.repo.SaveChecks(ctx, checks); err != nil {
			return RunSummary{}, fmt.Errorf("save checks: %w", err)
		}
	}

	// Время берём один раз, чтобы длительность и время окончания совпадали.
	finished := s.now()
	summary := RunSummary{
		Total:      len(checks),
		DurationMS: finished.Sub(start).Milliseconds(),
		FinishedAt: finished,
	}
	for _, c := range checks {
		if c.Up() {
			summary.Up++
		} else {
			summary.Down++
		}
	}

	// &summary — адрес локальной переменной. В Go это безопасно: компилятор
	// (escape analysis) сам разместит summary в куче, а не на стеке.
	s.mu.Lock()
	s.lastRun = &summary
	s.mu.Unlock()

	return summary, nil
}

// LastRun возвращает итог последнего завершённого прогона и true,
// либо пустой итог и false, если прогонов ещё не было (идиома «comma ok»).
func (s *Service) LastRun() (RunSummary, bool) {
	s.mu.Lock()
	defer s.mu.Unlock() // разблокировка гарантирована при любом return
	if s.lastRun == nil {
		return RunSummary{}, false
	}
	// Возвращаем КОПИЮ (*s.lastRun), а не указатель: вызывающий код не сможет
	// изменить внутреннее состояние сервиса в обход мьютекса.
	return *s.lastRun, true
}

// RunScheduler запускает прогон проверок каждые interval, пока не отменён ctx.
// Вызывается из main в отдельной горутине. interval <= 0 выключает планировщик.
func (s *Service) RunScheduler(ctx context.Context, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		log.Info("scheduler disabled")
		return
	}
	// Тикер присылает текущее время в канал ticker.C каждые interval.
	// Stop обязателен, чтобы освободить ресурсы тикера.
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Бесконечный цикл с select — типичная форма фоновой горутины:
	// ждём либо сигнала остановки, либо следующего тика.
	// Если прогон длится дольше interval, лишние тики не копятся:
	// у канала тикера буфер на одно значение, остальные отбрасываются.
	for {
		select {
		case <-ctx.Done():
			log.Info("scheduler stopped")
			return
		case <-ticker.C:
			summary, err := s.RunChecks(ctx)
			// switch без выражения — удобная замена цепочке if/else if.
			switch {
			case errors.Is(err, ErrCheckInProgress):
				// Прогон уже запущен вручную через API — пропускаем этот тик.
				log.Debug("scheduled run skipped: previous run still active")
			case err != nil:
				log.Error("scheduled run failed", "err", err)
			default:
				log.Info("scheduled run finished", "total", summary.Total, "up", summary.Up, "down", summary.Down)
			}
		}
	}
}
