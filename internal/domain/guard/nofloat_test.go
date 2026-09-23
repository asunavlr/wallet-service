// Package guard_test verifica, por varredura do código, a garantia que o
// enunciado trata como eliminatória: dinheiro não passa por float32 nem
// float64 — "nem durante parsing, cálculo, serialização ou persistência".
//
// Uma revisão de código prova isso hoje; este teste prova a cada commit.
package guard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pacotes onde nenhum float pode aparecer, em nenhuma forma.
var proibidos = []string{
	"../../domain",
	"../../app",
	"../../contract",
	"../../adapter/postgres",
	"../../adapter/httpapi",
	"../../adapter/sqs",
}

func TestNenhumFloatNoCaminhoDoDinheiro(t *testing.T) {
	fset := token.NewFileSet()

	for _, raiz := range proibidos {
		if _, err := os.Stat(raiz); os.IsNotExist(err) {
			continue // pacote ainda não escrito
		}
		err := filepath.Walk(raiz, func(caminho string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(caminho, ".go") {
				return err
			}
			arquivo, err := parser.ParseFile(fset, caminho, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(arquivo, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.Ident:
					// tipo float32/float64 declarado em qualquer posição
					if v.Name == "float32" || v.Name == "float64" {
						t.Errorf("%s: uso de %s no caminho do dinheiro",
							fset.Position(v.Pos()), v.Name)
					}
				case *ast.BasicLit:
					// literal de ponto flutuante, como 0.15
					if v.Kind == token.FLOAT {
						t.Errorf("%s: literal de ponto flutuante %s",
							fset.Position(v.Pos()), v.Value)
					}
				case *ast.SelectorExpr:
					// strconv.ParseFloat, json.Number.Float64, etc.
					if v.Sel != nil && strings.Contains(v.Sel.Name, "Float") {
						t.Errorf("%s: chamada a %s no caminho do dinheiro",
							fset.Position(v.Sel.Pos()), v.Sel.Name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("varrendo %s: %v", raiz, err)
		}
	}
}
