# logger-container

Interface web para visualizar logs de containers Docker em tempo real. Suporta streaming ao vivo via SSE, busca em histórico por data e filtragem por texto.

## Funcionalidades

- **Live streaming** — logs em tempo real via Server-Sent Events
- **Histórico** — busca por data com filtro de texto
- **Multi-container** — lista todos os containers (rodando ou parados)
- **Syntax highlighting** — destaque por nível: ERROR, WARN, INFO, DEBUG
- **Persistência** — logs salvos em arquivos JSON por data/container

## Uso rápido

### Docker Hub

```bash
docker run -d \
  --name logger \
  -p 8080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v ./logs:/app/logs \
  SEU_USUARIO/logger-container:latest
```

Acesse em `http://localhost:8080`.

### docker-compose.yml

```yaml
services:
  logger:
    image: SEU_USUARIO/logger-container:latest
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

### Direto do GitHub (sem Docker Hub)

```yaml
services:
  logger:
    build:
      context: https://github.com/SEU_USUARIO/logger-container.git
    ports:
      - "8080:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - ./logs:/app/logs
    restart: unless-stopped
```

O Docker builda a imagem direto do repositório público — não é necessário clonar.

Para fixar uma versão específica, use uma tag ou commit:

```yaml
build:
  context: https://github.com/SEU_USUARIO/logger-container.git#v1.0.0
```

## Variáveis de ambiente

| Variável   | Padrão      | Descrição                        |
|------------|-------------|----------------------------------|
| `PORT`     | `8080`      | Porta HTTP do servidor           |
| `LOGS_DIR` | `./logs`    | Diretório para salvar logs       |

## Publicando no Docker Hub

### 1. Build e push manual

```bash
docker build -t SEU_USUARIO/logger-container:latest .
docker push SEU_USUARIO/logger-container:latest
```

Para versionar:

```bash
docker build -t SEU_USUARIO/logger-container:v1.0.0 .
docker push SEU_USUARIO/logger-container:v1.0.0
docker tag SEU_USUARIO/logger-container:v1.0.0 SEU_USUARIO/logger-container:latest
docker push SEU_USUARIO/logger-container:latest
```

### 2. GitHub Actions (CI automático)

Crie `.github/workflows/docker.yml` no repositório:

```yaml
name: Docker

on:
  push:
    tags: ["v*"]

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: docker/login-action@v3
        with:
          username: ${{ secrets.DOCKERHUB_USERNAME }}
          password: ${{ secrets.DOCKERHUB_TOKEN }}

      - uses: docker/metadata-action@v5
        id: meta
        with:
          images: SEU_USUARIO/logger-container

      - uses: docker/build-push-action@v5
        with:
          push: true
          tags: ${{ steps.meta.outputs.tags }}
```

Configure os secrets no repositório:
- `DOCKERHUB_USERNAME` — seu usuário do Docker Hub
- `DOCKERHUB_TOKEN` — Access Token gerado em hub.docker.com → Account Settings → Security

Ao criar uma tag `git tag v1.0.0 && git push --tags`, o workflow builda e publica automaticamente.

## Desenvolvimento local

```bash
git clone https://github.com/SEU_USUARIO/logger-container
cd logger-container
docker compose up --build
```

## Volumes

| Path no container | Descrição                                      |
|-------------------|------------------------------------------------|
| `/app/logs`       | Logs persistidos em JSON (monte um volume aqui)|

O socket `/var/run/docker.sock` é necessário para listar containers e fazer streaming de logs.
