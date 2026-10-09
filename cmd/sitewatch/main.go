// Команда sitewatch — точка входа сервиса мониторинга сайтов.
//
// Здесь нет бизнес-логики: файл только «собирает» приложение из пакетов
// internal/* (создаёт объекты и передаёт их друг другу), запускает HTTP-сервер
// и фоновый планировщик и корректно останавливает их по сигналу.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"sitewatch/internal/checker"
	"sitewatch/internal/config"
	"sitewatch/internal/httpapi"
	"sitewatch/internal/postgres"
	"sitewatch/internal/site"
	"sitewatch/web"
)

// main — стартовая функция процесса. Создаёт логгер и передаёт управление run.
//
// Вся работа вынесена в run, потому что os.Exit завершает процесс немедленно,
// НЕ выполняя отложенные вызовы defer. Внутри run можно спокойно делать
// return err — все defer (закрытие пула БД и т.п.) отработают, — а os.Exit
// вызывается уже здесь, после выхода из run.
func main() {
	// Структурированный логгер из стандартной библиотеки: пишет строки вида
	// time=... level=INFO msg="..." key=value.
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1) // ненулевой код выхода сообщает ОС/Docker, что запуск не удался
	}
}

// run описывает всю жизнь программы: загрузка настроек, подключение к БД,
// миграции, сборка сервиса, запуск HTTP-сервера и планировщика, ожидание
// сигнала остановки и корректное (graceful) завершение.
// Возвращает ошибку, если что-то пошло не так; nil — при нормальной остановке.
func run(log *slog.Logger) error {
	// Файл .env необязателен: в продакшене переменные приходят из окружения.
	// godotenv не перезаписывает переменные, которые уже заданы в окружении.
	// Ошибку «файл не найден» игнорируем, любую другую (например, синтаксис) — нет.
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load .env: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		// %w «заворачивает» исходную ошибку: текст дополняется контекстом,
		// а errors.Is/As по-прежнему могут добраться до оригинала.
		return fmt.Errorf("config: %w", err)
	}

	// ctx отменяется при Ctrl+C (SIGINT) или SIGTERM (так Docker/Kubernetes
	// просят процесс завершиться) — это и запускает graceful shutdown.
	// stop отписывает процесс от сигналов; defer гарантирует вызов при выходе.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	// Отложенные вызовы выполняются в обратном порядке (LIFO), поэтому пул
	// закроется одним из последних — после остановки сервера и планировщика.
	defer pool.Close()

	applied, err := postgres.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	log.Info("migrations applied", "count", len(applied))

	// Ручное внедрение зависимостей: создаём реализации и передаём их сервису.
	// Сервис видит их только через интерфейсы site.Repository и site.Checker,
	// поэтому ничего не знает ни о PostgreSQL, ни о net/http.
	svc := site.NewService(
		postgres.NewRepository(pool),
		checker.New(cfg.CheckTimeout),
		cfg.Workers,
		cfg.RunTimeout,
	)

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: httpapi.NewRouter(svc, log, web.Static()),
		// Сколько ждать заголовки запроса. Защита от атаки Slowloris, когда
		// клиент шлёт заголовки по байту и держит соединения бесконечно.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// POST /api/checks/run ждёт окончания всего прогона, поэтому время на
		// запись ответа должно быть больше RUN_TIMEOUT.
		WriteTimeout: cfg.RunTimeout + 5*time.Second,
		// Сколько держать открытым простаивающее keep-alive соединение.
		IdleTimeout: time.Minute,
	}

	// WaitGroup нужна, чтобы при остановке дождаться выхода планировщика:
	// иначе run могла бы вернуться (и закрыть пул БД), пока он ещё пишет в базу.
	var wg sync.WaitGroup
	wg.Add(1) // Add вызывается ДО запуска горутины, иначе Wait может проскочить
	go func() {
		defer wg.Done()
		svc.RunScheduler(ctx, cfg.CheckInterval, log)
	}()

	// Канал с буфером на 1 значение: горутина сервера сможет положить ошибку
	// и завершиться, даже если её уже никто не читает (иначе — утечка горутины).
	serverErr := make(chan error, 1)
	go func() {
		log.Info("http server started", "addr", cfg.HTTPAddr)
		// ListenAndServe блокируется, пока сервер работает. После Shutdown он
		// возвращает http.ErrServerClosed — это нормальное завершение, не сбой.
		// Отдельная переменная listenErr: присваивание внешней err из этой
		// горутины было бы гонкой данных с главной горутиной.
		if listenErr := srv.ListenAndServe(); listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			serverErr <- listenErr
		}
	}()

	// Главная горутина ждёт, что случится раньше: сервер упадёт (например,
	// порт занят) или придёт сигнал остановки.
	select {
	case err = <-serverErr:
		stop() // отменяем ctx, чтобы остановить и планировщик
		wg.Wait()
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Для остановки нужен НОВЫЙ контекст: ctx уже отменён, и с ним Shutdown
	// завершился бы мгновенно, не дождавшись текущих запросов.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	// Shutdown перестаёт принимать новые соединения и ждёт завершения
	// уже начатых запросов (но не дольше SHUTDOWN_TIMEOUT).
	if err = srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	wg.Wait() // планировщик видит отменённый ctx и выходит из цикла
	log.Info("server stopped gracefully")
	return nil
}
