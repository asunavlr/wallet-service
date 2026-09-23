// Comando wallet: o serviço de carteira.
//
// O main é deliberadamente curto. Ele lê a configuração, monta a aplicação e
// entrega o controle ao Fx, que cuida do ciclo de vida. Toda a composição
// mora em internal/bootstrap.
package main

import (
	"fmt"
	"os"

	"github.com/kevinmatos/wallet-service/internal/bootstrap"
	"github.com/kevinmatos/wallet-service/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// Configuração inválida falha aqui, antes de qualquer conexão: subir
		// pela metade e falhar na primeira aposta é pior que não subir.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bootstrap.New(cfg).Run()
}
