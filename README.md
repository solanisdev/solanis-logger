# solanis-logger

Interface web para visualizar logs de containers Docker em tempo real. Suporta streaming ao vivo via SSE, busca em histórico por data e filtragem por texto.

## Funcionalidades

- **Live streaming** — logs em tempo real via Server-Sent Events
- **Histórico** — busca por data com filtro de texto
- **Multi-container** — lista todos os containers (rodando ou parados)
- **Syntax highlighting** — destaque por nível: ERROR, WARN, INFO, DEBUG
- **Persistência** — logs salvos em arquivos JSON por data/container

## Usando em outros projetos

Adicione o serviço ao `docker-compose.yml` do seu projeto:

```yaml
services:
  # ... seus outros serviços ...

  logger:
    build:
      context: https://github.com/solanisdev/solanis-logger.git
    ports:
      - "8080:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - ./logs:/app/logs
    environment:
      - LOGS_DIR=/app/logs
      - PORT=8080
    restart: unless-stopped
```

Suba com:

```bash
docker compose up -d
```

Acesse em `http://localhost:8080`. O logger vai listar automaticamente todos os containers do compose.

> O Docker baixa e builda a imagem direto do GitHub — não é necessário clonar o repositório.

## Variáveis de ambiente

| Variável   | Padrão   | Descrição                   |
|------------|----------|-----------------------------|
| `PORT`     | `8080`   | Porta HTTP do servidor      |
| `LOGS_DIR` | `./logs` | Diretório para salvar logs  |

## Volumes

| Path no container | Descrição                              |
|-------------------|----------------------------------------|
| `/app/logs`       | Logs persistidos em JSON por data      |

O socket `/var/run/docker.sock` (somente leitura) é necessário para listar containers e fazer streaming de logs.

## Desenvolvimento local

```bash
git clone https://github.com/solanisdev/solanis-logger
cd solanis-logger
docker compose up --build
```
