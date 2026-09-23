package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/domain/event"
	"github.com/kevinmatos/wallet-service/internal/domain/money"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
	"github.com/kevinmatos/wallet-service/internal/domain/wallet"
)

// Submit é o pedido de uma operação, já decodificado e validado na borda.
type Submit struct {
	ProviderID     string
	ExternalID     string
	IdempotencyKey string
	PlayerID       uuid.UUID
	WalletID       uuid.UUID
	RoundID        string
	GameID         string
	Kind           wager.Kind
	Money          money.Money
	ReferenceExtID string
	CorrelationID  string
	// Source distingue a entrada ("http" ou "sqs") para métrica e log.
	Source string
}

// Result é o que volta ao provedor.
type Result struct {
	TransactionID uuid.UUID
	Status        wager.Status
	Balance       *money.Money
	FailureCode   wager.FailureCode
	Replay        bool
}

// WagerService processa operações de aposta.
//
// HTTP e SQS chamam exatamente este caso de uso — é o que garante que as duas
// entradas compartilhem idempotência, regras e garantias financeiras, como o
// enunciado exige.
type WagerService struct {
	uow     UnitOfWork
	clock   Clock
	ids     IDGenerator
	metrics Metrics
	pending PendingPolicy
	retries int
}

// PendingPolicy governa a espera por referência.
type PendingPolicy struct {
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxAttempts int
}

// Backoff devolve o atraso da próxima tentativa: base × 2^tentativas,
// limitado ao máximo e sem estourar o deslocamento.
func (p PendingPolicy) Backoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 30 {
		return p.MaxDelay
	}
	d := p.BaseDelay << uint(attempts)
	if d <= 0 || d > p.MaxDelay {
		return p.MaxDelay
	}
	return d
}

// NewWagerService monta o caso de uso.
func NewWagerService(uow UnitOfWork, clock Clock, ids IDGenerator, m Metrics, p PendingPolicy, retries int) *WagerService {
	if retries < 1 {
		retries = 3
	}
	return &WagerService{uow: uow, clock: clock, ids: ids, metrics: m, pending: p, retries: retries}
}

// Submit processa uma operação, com idempotência persistente.
//
// Conflito de concorrência é refeito: na segunda passada a transação enxerga
// quem ganhou a corrida e responde como replay ou como conflito de chave.
func (s *WagerService) Submit(ctx context.Context, in Submit) (Result, error) {
	inicio := s.clock.Now()
	var res Result
	var err error
	for tentativa := 0; tentativa < s.retries; tentativa++ {
		res, err = s.submitOnce(ctx, in)
		if err == nil || !errors.Is(err, ErrConflict) {
			break
		}
		s.metrics.ConcurrencyConflict()
	}
	if err == nil {
		s.metrics.TransactionResult(string(in.Kind), string(res.Status), in.Source)
		s.metrics.ProcessingTime(in.Source, s.clock.Now().Sub(inicio))
		if res.Replay {
			s.metrics.Duplicate(in.Source)
		}
	}
	return res, err
}

func (s *WagerService) submitOnce(ctx context.Context, in Submit) (Result, error) {
	hash := wager.Fingerprint(wager.FingerprintInput{
		ProviderID:     in.ProviderID,
		ExternalID:     in.ExternalID,
		PlayerID:       in.PlayerID.String(),
		WalletID:       in.WalletID.String(),
		RoundID:        in.RoundID,
		GameID:         in.GameID,
		Kind:           in.Kind,
		Amount:         in.Money,
		ReferenceExtID: in.ReferenceExtID,
	})

	var out Result
	err := s.uow.Do(ctx, func(ctx context.Context, r *Repos) error {
		// 1. Trava a carteira ANTES de consultar idempotência.
		//
		// A ordem importa: travando primeiro, a busca de idempotência enxerga
		// tudo que outra instância já commitou para esta carteira. Consultar
		// antes de travar abriria uma janela entre "não achei" e "vou criar".
		w, err := r.Wallets.Lock(ctx, in.WalletID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("%w: carteira %s", ErrNotFound, in.WalletID)
			}
			return err
		}
		if w.PlayerID() != in.PlayerID {
			return fmt.Errorf("%w: carteira não pertence ao jogador informado", ErrInvalidInput)
		}

		// 2. Replay ou conflito de chave.
		if achada, err := r.Transactions.ByIdempotencyKey(ctx, in.ProviderID, in.IdempotencyKey); err == nil {
			if achada.PayloadHash() != hash {
				return ErrIdempotencyConflict
			}
			out = resultado(achada, true)
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}

		// 3. Mesma operação financeira chegando com outra chave.
		if _, err := r.Transactions.ByExternalID(ctx, in.ProviderID, in.ExternalID); err == nil {
			return ErrDuplicateExternalTransaction
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}

		// 4. Resolve a referência, quando declarada.
		var ref *wager.Reference
		var refID *uuid.UUID
		if in.ReferenceExtID != "" {
			achada, err := r.Transactions.ByExternalID(ctx, in.ProviderID, in.ReferenceExtID)
			switch {
			case err == nil:
				revertida, err := r.Transactions.HasSuccessfulReversal(ctx, achada.ID())
				if err != nil {
					return err
				}
				if d, ok := coerente(achada, in); !ok {
					ref = d
				} else {
					ref = &wager.Reference{
						Kind: achada.Kind(), Status: achada.Status(), Amount: achada.Amount(),
						RoundID: achada.RoundID(), Provider: achada.ProviderID(),
						PlayerID: achada.PlayerID().String(), WalletID: achada.WalletID().String(),
						Reversed: revertida,
					}
				}
				id := achada.ID()
				refID = &id
			case errors.Is(err, ErrNotFound):
				ref = nil // ainda não chegou
			default:
				return err
			}
		}

		// 5. Cria a transação e decide.
		tx, err := wager.NewExternal(wager.ExternalInput{
			ID: s.ids.New(), ProviderID: in.ProviderID, ExternalID: in.ExternalID,
			IdempotencyKey: in.IdempotencyKey, PayloadHash: hash,
			PlayerID: in.PlayerID, WalletID: in.WalletID,
			RoundID: in.RoundID, GameID: in.GameID, Kind: in.Kind, Amount: in.Money,
			ReferenceExtID: in.ReferenceExtID, CorrelationID: in.CorrelationID,
		}, s.clock.Now())
		if err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidInput, err)
		}

		decisao := wager.Decide(w, wager.Operation{
			Kind: in.Kind, Amount: in.Money, RoundID: in.RoundID, Provider: in.ProviderID,
		}, ref, in.ReferenceExtID != "")

		if err := s.aplicar(ctx, r, w, tx, decisao, refID); err != nil {
			return err
		}
		out = resultado(tx, false)
		return nil
	})
	return out, err
}

// coerente confere os vínculos que Decide não tem como ver, porque dependem
// dos identificadores resolvidos. Devolve ok=false e uma referência marcada
// para rejeição por divergência.
func coerente(ref *wager.Transaction, in Submit) (*wager.Reference, bool) {
	if ref.PlayerID() != in.PlayerID || ref.WalletID() != in.WalletID {
		// Uma referência de outro jogador ou carteira não é "não encontrada":
		// ela existe e não serve, e isso precisa ser distinguível.
		return &wager.Reference{
			Kind: ref.Kind(), Status: wager.Processed, Amount: ref.Amount(),
			RoundID: "divergente", Provider: ref.ProviderID(),
		}, false
	}
	return nil, true
}

// aplicar executa a decisão: movimenta (ou não), grava e enfileira os eventos.
//
// Tudo acontece dentro da transação SQL do caso de uso, então saldo, ledger,
// estado e eventos são confirmados no mesmo commit — que é o que torna
// impossível publicar antes de confirmar.
func (s *WagerService) aplicar(
	ctx context.Context, r *Repos, w *wallet.Wallet,
	tx *wager.Transaction, d wager.Decision, refID *uuid.UUID,
) error {
	agora := s.clock.Now()

	switch d.Action {
	case wager.ActionAwait:
		proxima := agora.Add(s.pending.Backoff(0))
		if err := tx.AwaitReference(proxima, agora); err != nil {
			return err
		}
		if err := r.Transactions.Insert(ctx, tx); err != nil {
			return err
		}
		return s.enfileirarPendente(ctx, r, tx)

	case wager.ActionReject:
		saldo := w.Balance()
		if err := tx.Reject(d.Code, &saldo, agora); err != nil {
			return err
		}
		if err := r.Transactions.Insert(ctx, tx); err != nil {
			return err
		}
		return s.enfileirarRejeitado(ctx, r, tx)

	case wager.ActionNoMove:
		// LOSS: conclui sem lançamento e sem versionar a carteira, mas emite
		// WagerTransactionProcessed. Nenhum WalletBalanceChanged, porque
		// nenhum saldo mudou.
		saldo := w.Balance()
		if err := tx.Process(saldo, refID, agora); err != nil {
			return err
		}
		if err := r.Transactions.Insert(ctx, tx); err != nil {
			return err
		}
		return s.enfileirarProcessado(ctx, r, tx, nil, w.Version())

	case wager.ActionMove:
		versaoAnterior := w.Version()
		lanc, err := w.Apply(s.ids.New(), tx.ID(), d.Direction, d.Amount, agora)
		if err != nil {
			// Saldo insuficiente detectado aqui seria bug de Decide, mas
			// rejeitar é melhor que abortar: o resultado fica auditável.
			if errors.Is(err, wallet.ErrInsufficientFunds) {
				saldo := w.Balance()
				if err := tx.Reject(wager.InsufficientFunds, &saldo, agora); err != nil {
					return err
				}
				if err := r.Transactions.Insert(ctx, tx); err != nil {
					return err
				}
				return s.enfileirarRejeitado(ctx, r, tx)
			}
			return err
		}
		if err := tx.Process(w.Balance(), refID, agora); err != nil {
			return err
		}
		// A transação é inserida antes do lançamento porque a FK composta do
		// ledger aponta para ela.
		if err := r.Transactions.Insert(ctx, tx); err != nil {
			return err
		}
		if err := r.Wallets.UpdateBalance(ctx, w, versaoAnterior); err != nil {
			return err
		}
		if err := r.Ledger.Insert(ctx, lanc); err != nil {
			return err
		}
		if err := s.enfileirarProcessado(ctx, r, tx, lanc, w.Version()); err != nil {
			return err
		}
		// Uma operação terminal pode destravar pendências que a esperavam.
		return r.Transactions.WakeWaitingFor(ctx, tx.ProviderID(), tx.ExternalID(), agora)
	}
	return fmt.Errorf("decisão desconhecida: %s", d.Action)
}

func (s *WagerService) enfileirarProcessado(
	ctx context.Context, r *Repos, tx *wager.Transaction,
	lanc *wallet.LedgerEntry, versao int64,
) error {
	saldo := tx.ResultBalance()
	ev, err := event.NewProcessed(s.ids.New(), tx.WalletID(), tx.CorrelationID(), tx.ID().String(), s.clock.Now(),
		event.ProcessedData{
			TransactionID: tx.ID(), WalletID: tx.WalletID(), PlayerID: tx.PlayerID(),
			ProviderID: tx.ProviderID(), ExternalID: tx.ExternalID(),
			RoundID: tx.RoundID(), GameID: tx.GameID(), Kind: string(tx.Kind()),
			Money: event.Of(tx.Amount()), Balance: event.Of(*saldo),
		})
	if err != nil {
		return err
	}
	eventos := []OutboxEvent{paraOutbox(ev, "Wallet", tx.WalletID())}

	// WalletBalanceChanged só existe quando o saldo efetivamente mudou.
	if lanc != nil {
		bc, err := event.NewBalanceChanged(s.ids.New(), tx.WalletID(), tx.CorrelationID(), tx.ID().String(), s.clock.Now(),
			event.BalanceChangedData{
				WalletID: tx.WalletID(), TransactionID: tx.ID(),
				Direction: string(lanc.Direction()), Money: event.Of(lanc.Amount()),
				BalanceBefore: event.Of(lanc.BalanceBefore()), BalanceAfter: event.Of(lanc.BalanceAfter()),
				WalletVersion: versao,
			})
		if err != nil {
			return err
		}
		eventos = append(eventos, paraOutbox(bc, "Wallet", tx.WalletID()))
	}
	return r.Outbox.Append(ctx, eventos...)
}

func (s *WagerService) enfileirarRejeitado(ctx context.Context, r *Repos, tx *wager.Transaction) error {
	ev, err := event.NewRejected(s.ids.New(), tx.WalletID(), tx.CorrelationID(), tx.ID().String(), s.clock.Now(),
		event.RejectedData{
			TransactionID: tx.ID(), WalletID: tx.WalletID(),
			ProviderID: tx.ProviderID(), ExternalID: tx.ExternalID(),
			Kind: string(tx.Kind()), Money: event.Of(tx.Amount()),
			FailureCode: string(tx.FailureCode()),
		})
	if err != nil {
		return err
	}
	return r.Outbox.Append(ctx, paraOutbox(ev, "Wallet", tx.WalletID()))
}

func (s *WagerService) enfileirarPendente(ctx context.Context, r *Repos, tx *wager.Transaction) error {
	ev, err := event.NewPendingReference(s.ids.New(), tx.WalletID(), tx.CorrelationID(), tx.ID().String(), s.clock.Now(),
		event.PendingReferenceData{
			TransactionID: tx.ID(), WalletID: tx.WalletID(),
			ProviderID: tx.ProviderID(), ExternalID: tx.ExternalID(),
			ReferenceExtID: tx.ReferenceExtID(), Kind: string(tx.Kind()),
			Attempts: tx.Attempts(), NextAttemptAt: tx.NextAttemptAt().UTC().Format(time.RFC3339Nano),
		})
	if err != nil {
		return err
	}
	return r.Outbox.Append(ctx, paraOutbox(ev, "Wallet", tx.WalletID()))
}

// paraOutbox converte o envelope no registro da outbox.
//
// PartitionKey é a carteira: é ela que dá a ordem que importa ao consumidor
// dos eventos, e agregados diferentes seguem em paralelo.
func paraOutbox(e event.Envelope, aggType string, agg uuid.UUID) OutboxEvent {
	bruto, _ := e.Marshal()
	at, _ := time.Parse(time.RFC3339Nano, e.OccurredAt)
	return OutboxEvent{
		EventID: e.EventID, EventType: e.EventType, EventVersion: e.Version,
		AggregateType: aggType, AggregateID: agg, PartitionKey: agg.String(),
		Payload: bruto, CorrelationID: e.CorrelationID, CausationID: e.CausationID,
		OccurredAt: at,
	}
}

func resultado(t *wager.Transaction, replay bool) Result {
	return Result{
		TransactionID: t.ID(), Status: t.Status(), Balance: t.ResultBalance(),
		FailureCode: t.FailureCode(), Replay: replay,
	}
}

// ─── retomada de pendências ─────────────────────────────────────────────────

// ResolvePending reavalia uma operação que esperava por referência.
//
// Mora aqui, e não no worker, porque é a MESMA decisão do fluxo síncrono: as
// regras não podem divergir entre quem chegou na hora certa e quem chegou
// antes da referência. O worker só empresta o laço e o agendamento.
//
// A ordem dos locks é a mesma do caminho HTTP — carteira primeiro —, porque
// locks adquiridos em ordens diferentes por caminhos diferentes é como nasce
// um deadlock.
func (s *WagerService) ResolvePending(ctx context.Context, r *Repos, id uuid.UUID) error {
	tx, err := r.Transactions.ByID(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // outra instância já tratou
		}
		return err
	}
	if tx.Status().Terminal() {
		return nil
	}

	w, err := r.Wallets.Lock(ctx, tx.WalletID())
	if err != nil {
		return err
	}
	agora := s.clock.Now()

	ref, err := r.Transactions.ByExternalID(ctx, tx.ProviderID(), tx.ReferenceExtID())
	switch {
	case errors.Is(err, ErrNotFound):
		// Ainda não chegou: adia, ou desiste quando o prazo se esgota.
		return s.adiar(ctx, r, tx, agora, wager.ReferenceNotFound)
	case err != nil:
		return err
	case ref.Status() == wager.PendingReference:
		// Existe, mas ela própria ainda espera. Continua aguardando: rejeitar
		// agora descartaria uma operação que ainda pode concluir.
		return s.adiar(ctx, r, tx, agora, wager.ReferenceNotProcessed)
	}

	// A referência chegou a um estado terminal: decide com ela em mãos.
	revertida, err := r.Transactions.HasSuccessfulReversal(ctx, ref.ID())
	if err != nil {
		return err
	}
	dominio := &wager.Reference{
		Kind: ref.Kind(), Status: ref.Status(), Amount: ref.Amount(),
		RoundID: ref.RoundID(), Provider: ref.ProviderID(),
		PlayerID: ref.PlayerID().String(), WalletID: ref.WalletID().String(),
		Reversed: revertida,
	}
	// Vínculos que Decide não enxerga porque dependem dos ids resolvidos.
	if ref.PlayerID() != tx.PlayerID() || ref.WalletID() != tx.WalletID() {
		dominio.RoundID = "divergente"
	}

	decisao := wager.Decide(w, wager.Operation{
		Kind: tx.Kind(), Amount: tx.Amount(), RoundID: tx.RoundID(), Provider: tx.ProviderID(),
	}, dominio, true)

	if decisao.Action == wager.ActionAwait {
		return s.adiar(ctx, r, tx, agora, wager.ReferenceNotProcessed)
	}

	refID := ref.ID()
	if err := s.aplicarRetomada(ctx, r, w, tx, decisao, &refID); err != nil {
		return err
	}
	s.metrics.TransactionResult(string(tx.Kind()), string(tx.Status()), "worker")
	return nil
}

// adiar agenda a próxima tentativa ou encerra como rejeição quando o número
// máximo de tentativas se esgota.
func (s *WagerService) adiar(ctx context.Context, r *Repos, tx *wager.Transaction, agora time.Time, motivo wager.FailureCode) error {
	if tx.Attempts() >= s.pending.MaxAttempts {
		w, err := r.Wallets.ByID(ctx, tx.WalletID())
		if err != nil {
			return err
		}
		saldo := w.Balance()
		if err := tx.Reject(motivo, &saldo, agora); err != nil {
			return err
		}
		if err := r.Transactions.Save(ctx, tx); err != nil {
			return err
		}
		s.metrics.TransactionResult(string(tx.Kind()), string(wager.Rejected), "worker")
		return s.enfileirarRejeitado(ctx, r, tx)
	}

	proxima := agora.Add(s.pending.Backoff(tx.Attempts()))
	if err := tx.AwaitReference(proxima, agora); err != nil {
		return err
	}
	s.metrics.Retry("reference")
	return r.Transactions.Save(ctx, tx)
}

// aplicarRetomada é aplicar para uma transação que JÁ EXISTE no banco: usa
// Save em vez de Insert, e o resto é idêntico.
func (s *WagerService) aplicarRetomada(
	ctx context.Context, r *Repos, w *wallet.Wallet,
	tx *wager.Transaction, d wager.Decision, refID *uuid.UUID,
) error {
	agora := s.clock.Now()

	switch d.Action {
	case wager.ActionReject:
		saldo := w.Balance()
		if err := tx.Reject(d.Code, &saldo, agora); err != nil {
			return err
		}
		if err := r.Transactions.Save(ctx, tx); err != nil {
			return err
		}
		return s.enfileirarRejeitado(ctx, r, tx)

	case wager.ActionNoMove:
		saldo := w.Balance()
		if err := tx.Process(saldo, refID, agora); err != nil {
			return err
		}
		if err := r.Transactions.Save(ctx, tx); err != nil {
			return err
		}
		return s.enfileirarProcessado(ctx, r, tx, nil, w.Version())

	case wager.ActionMove:
		versaoAnterior := w.Version()
		lanc, err := w.Apply(s.ids.New(), tx.ID(), d.Direction, d.Amount, agora)
		if err != nil {
			if errors.Is(err, wallet.ErrInsufficientFunds) {
				saldo := w.Balance()
				codigo := wager.InsufficientFunds
				if tx.Kind().IsReversal() {
					codigo = wager.ReversalInsufficientFunds
				}
				if err := tx.Reject(codigo, &saldo, agora); err != nil {
					return err
				}
				if err := r.Transactions.Save(ctx, tx); err != nil {
					return err
				}
				return s.enfileirarRejeitado(ctx, r, tx)
			}
			return err
		}
		if err := tx.Process(w.Balance(), refID, agora); err != nil {
			return err
		}
		if err := r.Transactions.Save(ctx, tx); err != nil {
			return err
		}
		if err := r.Wallets.UpdateBalance(ctx, w, versaoAnterior); err != nil {
			return err
		}
		if err := r.Ledger.Insert(ctx, lanc); err != nil {
			return err
		}
		if err := s.enfileirarProcessado(ctx, r, tx, lanc, w.Version()); err != nil {
			return err
		}
		return r.Transactions.WakeWaitingFor(ctx, tx.ProviderID(), tx.ExternalID(), agora)
	}
	return fmt.Errorf("decisão desconhecida na retomada: %s", d.Action)
}

// PendingPolicyOf expõe a política, para que o worker use o mesmo backoff.
func (s *WagerService) PendingPolicyOf() PendingPolicy { return s.pending }
