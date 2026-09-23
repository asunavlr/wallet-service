package app

import "errors"

// A classificação de erro desta camada decide três coisas de uma vez: se a
// transação SQL é refeita, se a mensagem do SQS volta para a fila ou vai para
// a DLQ, e qual código HTTP o provedor recebe.
//
// A distinção que mais importa é entre falha TRANSITÓRIA (tentar de novo pode
// dar certo) e PERMANENTE (tentar de novo dá o mesmo resultado). Rejeição de
// negócio não é nenhuma das duas: é sucesso do ponto de vista da mensageria,
// e por isso remove a mensagem da fila.
var (
	// ErrConflict é transitório: violação de unicidade, versão desatualizada,
	// deadlock ou serialização. A operação é refeita e, na segunda passada,
	// enxerga quem ganhou a corrida.
	ErrConflict = errors.New("app: conflito de concorrência")

	// ErrUnavailable é transitório: o banco ou o SQS não respondeu. Retry com
	// backoff; nada de DLQ.
	ErrUnavailable = errors.New("app: dependência indisponível")

	// ErrNotFound indica recurso inexistente.
	ErrNotFound = errors.New("app: não encontrado")

	// ErrIdempotencyConflict indica a mesma chave com conteúdo diferente.
	// É permanente: reenviar não muda nada, o cliente precisa corrigir.
	ErrIdempotencyConflict = errors.New("app: chave de idempotência reutilizada com outro conteúdo")

	// ErrDuplicateExternalTransaction indica a mesma operação financeira
	// chegando com outra chave de idempotência.
	ErrDuplicateExternalTransaction = errors.New("app: operação já registrada com outra chave")

	// ErrForbidden indica provedor tentando alcançar dado de outro provedor.
	ErrForbidden = errors.New("app: acesso negado")

	// ErrInvalidInput indica entrada malformada. Permanente e corrigível: não
	// é persistida, o cliente corrige e reenvia com a mesma chave.
	ErrInvalidInput = errors.New("app: entrada inválida")
)

// Transient informa se vale a pena tentar de novo.
func Transient(err error) bool {
	return errors.Is(err, ErrConflict) || errors.Is(err, ErrUnavailable)
}
