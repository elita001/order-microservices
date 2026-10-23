// Package smtp implements the email delivery adapter over SMTP.
package smtp

import (
	"context"
	"fmt"
	"time"

	"github.com/wneessen/go-mail"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/elita001/order-microservices/internal/notification/application"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

var tracer = telemetry.Tracer("github.com/elita001/order-microservices/internal/notification/adapter/smtp")

// Config is usually loaded from environment variables with the SMTP_ prefix.
type Config struct {
	Host     string        `env:"HOST" envDefault:"localhost"`
	Port     int           `env:"PORT" envDefault:"1025"`
	Username string        `env:"USERNAME"` // empty: no authentication
	Password string        `env:"PASSWORD"`
	TLS      bool          `env:"TLS" envDefault:"false"` // require STARTTLS; Mailpit in development has no TLS
	From     string        `env:"FROM" envDefault:"OrderFlow <noreply@orderflow.local>"`
	Timeout  time.Duration `env:"TIMEOUT" envDefault:"10s"`
}

type Mailer struct {
	cfg  Config
	opts []mail.Option
}

var _ application.Mailer = (*Mailer)(nil)

func NewMailer(cfg Config) *Mailer {
	tlsPolicy := mail.NoTLS
	if cfg.TLS {
		tlsPolicy = mail.TLSMandatory
	}
	opts := []mail.Option{mail.WithPort(cfg.Port), mail.WithTimeout(cfg.Timeout), mail.WithTLSPolicy(tlsPolicy)}
	if cfg.Username != "" {
		opts = append(opts, mail.WithSMTPAuth(mail.SMTPAuthPlain), mail.WithUsername(cfg.Username), mail.WithPassword(cfg.Password))
	}
	return &Mailer{cfg: cfg, opts: opts}
}

// Send opens a new SMTP connection for every email. That is simple and safe for
// concurrent use; a connection pool is only worth it at much higher volumes.
func (m *Mailer) Send(ctx context.Context, email application.Email) (err error) {
	ctx, span := tracer.Start(ctx, "smtp.send", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("server.address", m.cfg.Host)))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "send failed")
		}
		span.End()
	}()

	msg := mail.NewMsg()
	if err := msg.From(m.cfg.From); err != nil {
		return fmt.Errorf("set sender: %w", err)
	}
	if err := msg.To(email.To); err != nil {
		return fmt.Errorf("%w: %w", application.ErrInvalidRecipient, err)
	}
	msg.Subject(email.Subject)
	msg.SetDate()
	msg.SetMessageID()
	msg.SetBodyString(mail.TypeTextPlain, email.Text)
	if email.HTML != "" {
		msg.AddAlternativeString(mail.TypeTextHTML, email.HTML)
	}

	client, err := mail.NewClient(m.cfg.Host, m.opts...)
	if err != nil {
		return fmt.Errorf("create smtp client: %w", err)
	}
	if err := client.DialAndSendWithContext(ctx, msg); err != nil {
		return fmt.Errorf("send email: %w", err)
	}
	return nil
}
