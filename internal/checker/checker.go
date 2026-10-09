// Пакет checker проверяет доступность URL по HTTP.
// Тип Checker реализует интерфейс site.Checker.
package checker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxRedirects — сколько перенаправлений (301/302) разрешено пройти.
// Защищает от бесконечных циклов редиректов.
const maxRedirects = 5

// Checker выполняет HTTP-проверки.
//
// Один экземпляр безопасно используют все воркеры одновременно: http.Client
// и его Transport рассчитаны на конкурентное использование и переиспользуют
// TCP-соединения между запросами.
type Checker struct {
	client  *http.Client
	timeout time.Duration // лимит времени одной проверки
}

// New создаёт Checker, одна проверка которого никогда не длится дольше timeout.
//
// Глобальный http.DefaultClient не используется: у него нет таймаута, и запрос
// к зависшему серверу мог бы висеть бесконечно.
func New(timeout time.Duration) *Checker {
	// Берём копию стандартного транспорта: в нём уже есть разумные таймауты
	// соединения, поддержка прокси и HTTP/2. Конструкция .(*http.Transport) —
	// утверждение типа: DefaultTransport объявлен как интерфейс RoundTripper.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// По умолчанию Go держит лишь 2 простаивающих соединения на хост.
	// Больше — меньше новых TCP/TLS-рукопожатий при частых проверках.
	transport.MaxIdleConnsPerHost = 10

	return &Checker{
		timeout: timeout,
		client: &http.Client{
			Transport: transport,
			// Клиент вызывает эту функцию перед каждым редиректом;
			// via — запросы, уже выполненные в этой цепочке.
			// Возврат ошибки останавливает переходы.
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("stopped after %d redirects", maxRedirects)
				}
				return nil
			},
		},
	}
}

// Check выполняет GET-запрос по url и возвращает HTTP-код ответа и задержку.
//
// Код не из диапазона 2xx — НЕ ошибка: сайт ответил, просто «нездоров».
// Ошибка возвращается, только когда ответа не было вовсе (DNS, отказ
// соединения, TLS, таймаут). Решение UP/DOWN принимает site.Check.Up().
func (c *Checker) Check(ctx context.Context, url string) (int, time.Duration, error) {
	// Собственный таймаут проверки поверх контекста прогона: сработает то,
	// что наступит раньше. Дочерний контекст сохранён в отдельную переменную
	// probeCtx, чтобы ctx вызывающего остался доступен — по нему ниже видно,
	// истёк ли таймаут проверки или отменён весь прогон.
	probeCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// Запрос привязан к контексту: при его отмене соединение будет прервано.
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("build request: %w", err)
	}
	// Хороший тон: владелец сайта видит в логах, кто к нему обращается.
	req.Header.Set("User-Agent", "sitewatch/1.0")

	start := time.Now()
	resp, err := c.client.Do(req)
	latency := time.Since(start)
	if err != nil {
		// client.Do возвращает *url.Error, у которого есть Unwrap(), поэтому
		// errors.Is находит внутри context.DeadlineExceeded.
		// ctx.Err() == nil означает, что контекст прогона ещё жив, — значит,
		// истёк именно таймаут этой проверки, и можно дать понятное сообщение.
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return 0, latency, fmt.Errorf("timeout after %s", c.timeout)
		}
		return 0, latency, err
	}
	// Тело ответа ОБЯЗАТЕЛЬНО закрывать, иначе утекают соединения.
	defer resp.Body.Close()

	// Соединение можно вернуть в пул и переиспользовать, только если тело
	// прочитано до конца. Дочитываем его, но не больше 64 КБ (64<<10 = 65536),
	// чтобы не скачивать огромные страницы. Результат явно игнорируем (_, _).
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	return resp.StatusCode, latency, nil
}
