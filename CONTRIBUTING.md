# Como contribuir

Obrigado pelo interesse no llmwatch! Contribuições são bem-vindas: correções,
novos provedores, melhorias no painel e na documentação.

## Antes de começar

- **Bug ou ideia?** Abra uma [issue](https://github.com/fthiago01/llmwatch/issues/new/choose)
  antes de escrever código grande, para alinharmos a abordagem.
- **Mudança pequena** (typo, correção óbvia)? Pode mandar o pull request direto.

## Ambiente

Requer **Go 1.25+**. Não há outras dependências: o projeto é Go puro, sem CGO.

```bash
git clone https://github.com/<seu-usuario>/llmwatch.git
cd llmwatch
go run .          # sobe em http://localhost:18123
```

Use `LLMWATCH_DATA_DIR` para não misturar com a sua instalação:

```bash
LLMWATCH_DATA_DIR=/tmp/llmwatch-dev LLMWATCH_ADDR=127.0.0.1:18199 go run .
```

## Fluxo

1. Faça um fork e crie uma branch a partir da `main` (`feat/novo-provider`, `fix/grafico-24h`).
2. Faça a mudança com testes quando houver lógica nova.
3. Rode localmente o mesmo que o CI roda:

   ```bash
   go vet ./...
   go test -race ./...
   ```

4. Abra o pull request preenchendo o template. Para mudanças no painel, inclua um print.

Todo pull request precisa do CI verde e da aprovação do mantenedor antes do merge.

## Padrões

- **Commits** no formato `tipo: descrição` em português — `feat:`, `fix:`, `docs:`, `refactor:`, `test:`.
- **Código Go** formatado com `gofmt` e comentários em inglês, seguindo o estilo do arquivo que você está editando.
- **Painel** (`web/`): HTML, CSS e JavaScript puros, sem bibliotecas nem requisições externas.
  Cores sempre pelos tokens CSS (`var(--text)`, `var(--panel)`…), para funcionar nos temas claro e escuro.
- **Sem dependências novas** sem discutir antes numa issue.
- **Privacidade**: o llmwatch é local. Nada de telemetria ou envio de dados para fora da máquina.

## Licença

Ao contribuir, você concorda que a sua contribuição será licenciada sob a [MIT](LICENSE).
