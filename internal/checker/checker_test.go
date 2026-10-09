package checker

// Тесты HTTP-проверки. Вместо настоящих сайтов используется httptest.NewServer —
// реальный HTTP-сервер на случайном локальном порту, поведение которого
// задаёт тест. Так тесты быстрые и не зависят от интернета.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCheck_StatusCodes проверяет, что Check возвращает код ответа сервера
// и что ответы 4xx/5xx не считаются ошибкой функции.
func TestCheck_StatusCodes(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantStatus int
	}{
		{"ok", http.StatusOK, http.StatusOK},
		{"server error is not a transport error", http.StatusInternalServerError, http.StatusInternalServerError},
		{"not found", http.StatusNotFound, http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Сервер, который всегда отвечает кодом tt.status.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			status, _, err := New(time.Second).Check(context.Background(), srv.URL)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %d, want %d", status, tt.wantStatus)
			}
		})
	}
}

// TestCheck_Timeout проверяет, что зависший сервер не задерживает проверку
// дольше её таймаута и что ошибка сообщает именно о таймауте.
func TestCheck_Timeout(t *testing.T) {
	// Сервер «думает» 2 секунды (или пока клиент не отключится).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	_, latency, err := New(50*time.Millisecond).Check(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	// Если таймаут не сработал, проверка длилась бы около 2 секунд.
	if latency > time.Second {
		t.Errorf("check took %v, the timeout was not applied", latency)
	}
}

// TestCheck_CallerDeadline: если первым истекает контекст вызывающего (то есть
// закончилось время всего прогона), ошибка не должна утверждать, что сработал
// таймаут самой проверки.
func TestCheck_CallerDeadline(t *testing.T) {
	// Сервер не отвечает, пока клиент не отключится.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	// Контекст «прогона» истекает через 50 мс — намного раньше таймаута проверки (5 с).
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := New(5*time.Second).Check(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if strings.Contains(err.Error(), "timeout after") {
		t.Errorf("err = %q, must not report the probe timeout", err)
	}
}

// TestCheck_TooManyRedirects проверяет защиту от бесконечных перенаправлений.
func TestCheck_TooManyRedirects(t *testing.T) {
	// srv объявлен заранее, чтобы обработчик мог сослаться на srv.URL:
	// сервер перенаправляет сам на себя по кругу.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/loop", http.StatusFound)
	}))
	defer srv.Close()

	if _, _, err := New(time.Second).Check(context.Background(), srv.URL); err == nil {
		t.Fatal("expected redirect error, got nil")
	}
}
