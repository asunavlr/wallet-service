package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/domain/event"
	"github.com/kevinmatos/wallet-service/internal/domain/money"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
	"github.com/kevinmatos/wallet-service/internal/domain/wallet"
)

// WalletService abre carteiras e reconcilia saldos.
type WalletService struct {
	uow     UnitOfWork
	clock   Clock
	ids     IDGenerator
	metrics Metrics
	log     *slog.Logger
}

func NewWalletService(uow UnitOfWork, clock Clock, ids IDGenerator, m Metrics, log *slog.Logger) *WalletService {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &WalletService{uow: uow, clock: clock, ids: ids, metrics: m, log: log}
}

// OpenWallet é o pedido de abertura.
type OpenWallet struct {
	PlayerID      uuid.UUID
	Initial       money.Money
	CorrelationID string
}

// Open cria a carteira e, havendo saldo inicial, o crédito de abertura.
//
// Com saldo positivo, a carteira, a OPENING em PROCESSED, o lançamento de
// crédito e os dois eventos saem no MESMO commit. Com saldo zero, não há
// OPENING, não há lançamento e não há evento financeiro — o enunciado é
// explícito nos dois casos. A versão da carteira é 1 de qualquer forma.
func (s *WalletService) Open(ctx context.Context, in OpenWallet) (*wallet.Wallet, error) {
	agora := s.clock.Now()
	var criada *wallet.Wallet

	err := s.uow.Do(ctx, func(ctx context.Context, r *Repos) error {
		w, err := wallet.Open(s.ids.New(), in.PlayerID, in.Initial, agora)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidInput, err)
		}
		if err := r.Wallets.Insert(ctx, w); err != nil {
			return err // unicidade (jogador, moeda) vira ErrConflict no adaptador
		}

		if in.Initial.IsPositive() {
			tx, err := wager.NewOpening(s.ids.New(), w.ID(), in.PlayerID, in.Initial, in.CorrelationID, agora)
			if err != nil {
				return err
			}
			if err := r.Transactions.Insert(ctx, tx); err != nil {
				return err
			}
			zero, err := money.Zero(in.Initial.Currency())
			if err != nil {
				return err
			}
			lanc, err := wallet.NewLedgerEntry(s.ids.New(), w.ID(), tx.ID(),
				wallet.Credit, in.Initial, zero, in.Initial, agora)
			if err != nil {
				return err
			}
			if err := r.Ledger.Insert(ctx, lanc); err != nil {
				return err
			}
			if err := s.eventosDeAbertura(ctx, r, w, tx, lanc); err != nil {
				return err
			}
		}
		criada = w
		return nil
	})
	if err != nil {
		return nil, err
	}
	return criada, nil
}

func (s *WalletService) eventosDeAbertura(
	ctx context.Context, r *Repos, w *wallet.Wallet,
	tx *wager.Transaction, lanc *wallet.LedgerEntry,
) error {
	agora := s.clock.Now()
	// Eventos de origem interna não carregam os metadados externos
	// inaplicáveis — provedor, rodada e jogo ficam vazios e são omitidos.
	pr, err := event.NewProcessed(s.ids.New(), w.ID(), tx.CorrelationID(), tx.ID().String(), agora,
		event.ProcessedData{
			TransactionID: tx.ID(), WalletID: w.ID(), PlayerID: w.PlayerID(),
			Kind: string(wager.Opening), Money: event.Of(tx.Amount()), Balance: event.Of(w.Balance()),
		})
	if err != nil {
		return err
	}
	bc, err := event.NewBalanceChanged(s.ids.New(), w.ID(), tx.CorrelationID(), tx.ID().String(), agora,
		event.BalanceChangedData{
			WalletID: w.ID(), TransactionID: tx.ID(), Direction: string(wallet.Credit),
			Money: event.Of(lanc.Amount()), BalanceBefore: event.Of(lanc.BalanceBefore()),
			BalanceAfter: event.Of(lanc.BalanceAfter()), WalletVersion: w.Version(),
		})
	if err != nil {
		return err
	}
	return r.Outbox.Append(ctx,
		paraOutbox(pr, "Wallet", w.ID()),
		paraOutbox(bc, "Wallet", w.ID()),
	)
}

// Reconciliation é o resultado da conferência entre saldo e ledger.
type Reconciliation struct {
	WalletID       uuid.UUID
	Stored         money.Money
	Calculated     money.Money
	Difference     money.Money
	Consistent     bool
	CheckedEntries int
}

// Reconcile reconstrói o saldo a partir do ledger e compara com o guardado.
//
// Não corrige nada: o enunciado é explícito que a reconciliação não altera o
// saldo. Divergência é reportada na resposta, no log e numa métrica — corrigir
// exigiria novo lançamento, porque o ledger é append-only.
//
// A leitura acontece dentro de uma transação para que saldo e ledger venham
// da mesma visão consistente; ler fora daria uma diferença falsa se uma
// operação commitasse no meio.
func (s *WalletService) Reconcile(ctx context.Context, walletID uuid.UUID) (Reconciliation, error) {
	var out Reconciliation
	err := s.uow.Do(ctx, func(ctx context.Context, r *Repos) error {
		w, err := r.Wallets.ByID(ctx, walletID)
		if err != nil {
			return err
		}
		calculado, n, err := r.Ledger.Balance(ctx, walletID)
		if err != nil {
			return err
		}
		// difference = saldo armazenado menos saldo reconstruído
		dif, err := w.Balance().Sub(calculado)
		if err != nil {
			return err
		}
		out = Reconciliation{
			WalletID: walletID, Stored: w.Balance(), Calculated: calculado,
			Difference: dif, Consistent: dif.IsZero(), CheckedEntries: n,
		}
		return nil
	})
	if err != nil {
		return Reconciliation{}, err
	}
	if !out.Consistent {
		// O enunciado pede a divergência reportada em TRÊS lugares: na
		// resposta, no log e numa métrica. A resposta é o retorno; a métrica
		// dispara o alarme; o log é o que permite investigar qual carteira,
		// quando, e de quanto foi a diferença.
		s.metrics.ReconciliationDivergence()
		s.log.LogAttrs(ctx, slog.LevelError, "divergência de reconciliação",
			slog.String("walletId", walletID.String()),
			slog.String("storedBalance", out.Stored.String()),
			slog.String("calculatedBalance", out.Calculated.String()),
			slog.String("difference", out.Difference.String()),
			slog.Int("checkedEntries", out.CheckedEntries))
	}
	return out, nil
}

// Get devolve a carteira.
func (s *WalletService) Get(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	var w *wallet.Wallet
	err := s.uow.Do(ctx, func(ctx context.Context, r *Repos) error {
		achada, err := r.Wallets.ByID(ctx, id)
		if err != nil {
			return err
		}
		w = achada
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, err
	}
	return w, nil
}

// LedgerPage devolve uma página do ledger com cursor opaco.
func (s *WalletService) LedgerPage(ctx context.Context, id uuid.UUID, cursor string, limit int) ([]LedgerRow, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var linhas []LedgerRow
	var proximo string
	err := s.uow.Do(ctx, func(ctx context.Context, r *Repos) error {
		if _, err := r.Wallets.ByID(ctx, id); err != nil {
			return err
		}
		l, c, err := r.Ledger.Page(ctx, id, cursor, limit)
		if err != nil {
			return err
		}
		linhas, proximo = l, c
		return nil
	})
	return linhas, proximo, err
}
