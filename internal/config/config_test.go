package config

// Тесты пакета config. Переменные окружения задаются через t.Setenv:
// он устанавливает значение только на время теста и сам восстанавливает
// прежнее после его окончания.

import (
	"testing"
	"time"
)

// TestLoad_Defaults проверяет, что при пустых переменных подставляются
// значения по умолчанию.
func TestLoad_Defaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test") // обязательная переменная
	t.Setenv("WORKERS", "")
	t.Setenv("CHECK_TIMEOUT", "")

	cfg, err := Load()
	if err != nil {
		// Fatalf прерывает тест: дальше проверять нечего.
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Workers != 5 {
		// Errorf отмечает тест как упавший, но выполнение продолжается.
		t.Errorf("Workers = %d, want 5", cfg.Workers)
	}
	if cfg.CheckTimeout != 5*time.Second {
		t.Errorf("CheckTimeout = %v, want 5s", cfg.CheckTimeout)
	}
}

// TestLoad_Errors — табличный тест: для каждого набора некорректных
// переменных Load должна вернуть ошибку.
func TestLoad_Errors(t *testing.T) {
	// Таблица случаев: срез анонимных структур. Добавить новый случай —
	// значит добавить одну строку.
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"missing database url", map[string]string{"DATABASE_URL": ""}},
		{"workers not a number", map[string]string{"WORKERS": "many"}},
		{"workers out of range", map[string]string{"WORKERS": "0"}},
		{"bad duration", map[string]string{"CHECK_TIMEOUT": "5 parsecs"}},
		{"run timeout too small", map[string]string{"CHECK_TIMEOUT": "10s", "RUN_TIMEOUT": "1s"}},
	}

	for _, tt := range tests {
		// t.Run запускает подтест с именем: в выводе будет видно, какой
		// именно случай упал, и его можно запустить отдельно через -run.
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://localhost/test")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
