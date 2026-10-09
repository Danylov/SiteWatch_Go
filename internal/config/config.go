// Пакет config загружает настройки приложения из переменных окружения.
//
// Сам файл .env пакет не читает — это делает main до вызова Load. Так пакет
// проще тестировать: в тестах переменные задаются через t.Setenv.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config — все настройки приложения.
// Структура маленькая и после старта не меняется, поэтому передаётся
// по значению (копией), а не по указателю.
type Config struct {
	HTTPAddr        string        // адрес HTTP-сервера, например ":8080"
	DatabaseURL     string        // строка подключения к PostgreSQL
	Workers         int           // сколько сайтов проверяется одновременно
	CheckTimeout    time.Duration // таймаут одной HTTP-проверки
	RunTimeout      time.Duration // таймаут всего прогона проверок
	CheckInterval   time.Duration // период фоновых проверок; 0 выключает планировщик
	ShutdownTimeout time.Duration // сколько ждать завершения запросов при остановке
}

// Load собирает Config из переменных окружения, подставляет значения
// по умолчанию и проверяет результат. Значения из .env к этому моменту
// уже должны быть загружены в окружение вызывающим кодом.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:    getEnv("HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"), // без умолчания: обязательна
	}

	// err объявлена заранее, поэтому ниже используется «=», а не «:=».
	// Приём «присвоить и проверить в одной строке»: if x, err = f(); err != nil.
	var err error
	if cfg.Workers, err = getInt("WORKERS", 5); err != nil {
		return Config{}, err
	}
	if cfg.CheckTimeout, err = getDuration("CHECK_TIMEOUT", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.RunTimeout, err = getDuration("RUN_TIMEOUT", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.CheckInterval, err = getDuration("CHECK_INTERVAL", time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = getDuration("SHUTDOWN_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}

	if err = cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate проверяет допустимость значений и их взаимные ограничения.
// Ошибки не возвращаются по одной, а собираются вместе: пользователь сразу
// видит все проблемы конфигурации, а не исправляет их по одной за запуск.
func (c Config) validate() error {
	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.Workers < 1 || c.Workers > 100 {
		errs = append(errs, fmt.Errorf("WORKERS must be in [1, 100], got %d", c.Workers))
	}
	if c.CheckTimeout <= 0 {
		errs = append(errs, errors.New("CHECK_TIMEOUT must be positive"))
	}
	if c.RunTimeout < c.CheckTimeout {
		errs = append(errs, errors.New("RUN_TIMEOUT must not be less than CHECK_TIMEOUT"))
	}
	// errors.Join объединяет ошибки в одну (каждая — с новой строки).
	// Для пустого среза возвращает nil, то есть «ошибок нет».
	return errors.Join(errs...)
}

// getEnv возвращает значение переменной окружения key или def,
// если переменная не задана либо пустая.
func getEnv(key, def string) string {
	// os.LookupEnv, в отличие от os.Getenv, сообщает ещё и ok — была ли
	// переменная задана вообще.
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// getInt читает целое число из переменной окружения key.
// Пустое значение — значит def; нечисловое — ошибка с именем переменной.
func getInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		// Пример текста: WORKERS: strconv.Atoi: parsing "many": invalid syntax
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// getDuration читает длительность из переменной окружения key.
// Формат — как у time.ParseDuration: "300ms", "5s", "1m30s", "2h".
// Пустое значение — значит def.
func getDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
