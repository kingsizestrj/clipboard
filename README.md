# Clip — área de transferência compartilhada

Um app web minúsculo e self-hosted pra **copiar e colar texto, imagens e arquivos entre dispositivos** (PC ↔ celular, celular ↔ celular, PC ↔ PC). Abra a página em qualquer aparelho, jogue algo lá, e aparece na hora nos outros pra copiar ou baixar.

- **Leve:** binário Go estático, sem dependências externas, imagem Docker pequena. RAM em repouso ~10–20 MB. Ideal pra homelab.
- **Sem instalar nada no celular:** é só abrir a URL (e dá pra "Adicionar à tela inicial" como PWA).
- **Tempo real:** novos itens aparecem instantaneamente (Server-Sent Events).
- **Seguro pra expor:** PIN, rate-limit no login, limites de upload, proteção contra path traversal e XSS.
- **Não depende de cookie:** o login guarda um token no `localStorage` e o envia por header `Authorization` (e por `?t=` no streaming/downloads). Funciona mesmo em navegadores que bloqueiam cookies. O cookie ainda é enviado como bônus onde for permitido. (O token aparece na URL de alguns recursos internos, o que pode constar no log do servidor — aceitável num homelab.)

## Como funciona

| Ação | Como |
|------|------|
| Enviar texto | Cola/digita na caixa e "Enviar texto" (ou Ctrl/Cmd+Enter) |
| Enviar imagem (desktop) | Cola com Ctrl/Cmd+V em qualquer lugar da página |
| Enviar arquivo/foto (celular) | Botão "Anexar / foto" |
| Arrastar e soltar | Solta o arquivo na janela |
| Copiar texto | Botão "Copiar" no item |
| Copiar imagem | Botão "Copiar imagem" (cola direto em apps que aceitam) |
| Baixar | Link "Baixar" |

## Deploy no host (192.168.18.238)

Copie a pasta `clip/` para o servidor e rode:

```bash
cd clip
cp .env.example .env
nano .env            # defina um CLIP_PIN forte
docker compose up -d --build
```

Acesse em `http://192.168.18.238:8099`. Pronto.

> O app **não sobe** se `CLIP_PIN` estiver vazio ou no valor de exemplo — proteção contra deixar sem senha por acidente. Para rodar de propósito sem login numa rede confiável, suba com `CLIP_ALLOW_NO_PIN=1`.

### Variáveis de ambiente

| Variável | Padrão | O que faz |
|----------|--------|-----------|
| `CLIP_PIN` | *(obrigatório)* | PIN de acesso. Trocar o PIN derruba as sessões antigas. |
| `CLIP_ALLOW_NO_PIN` | `0` | `1` permite subir sem PIN (sem autenticação). Use só em rede isolada. |
| `CLIP_TRUST_PROXY` | `0` | `1` faz confiar no `X-Forwarded-For` (só atrás de proxy/túnel que sanitiza esse header). |
| `PORT` | `8080` | Porta interna do container. |
| `CLIP_MAX_UPLOAD_MB` | `64` | Tamanho máximo por arquivo. |
| `CLIP_MAX_TEXT_KB` | `1024` | Tamanho máximo de um item de texto. |
| `CLIP_RETENTION_DAYS` | `14` | Apaga itens mais velhos que isso (`0` = nunca). |
| `CLIP_MAX_ITEMS` | `300` | Mantém só os N itens mais recentes (`0` = ilimitado). |

Os dados ficam no volume `clip-data` (`/data` dentro do container): índice em `index.json`, arquivos em `blobs/`, segredo da sessão em `.secret`.

O `docker-compose.yml` já vem com limites frugais (`mem_limit: 256m`, `cpus: 0.5`, `pids_limit`, `cap_drop: ALL`, `no-new-privileges`, rootfs read-only) — bom pra um host compartilhado.

> **Copiar em HTTP puro:** o botão "Copiar" usa a Clipboard API do navegador, que só existe em HTTPS ou `localhost`. Acessando por `http://IP` na LAN, o app cai num fallback (seleciona o texto / `execCommand`) que funciona, mas no celular pode exigir copiar manualmente. Para o "Copiar" 1-clique perfeito no celular, acesse via HTTPS (Tailscale Serve ou Cloudflare Tunnel — veja abaixo).

## Hostname bonito no AdGuard (opcional)

No AdGuard Home → **Filters → DNS rewrites**, adicione (igual você já fez com `git.berimbolo.home`):

- Domínio: `clip.berimbolo.home`
- IP: `192.168.18.238`

Aí acessa por `http://clip.berimbolo.home:8099`.

## Acesso de fora de casa

Escolha **um**:

### Opção A — Tailscale (mais simples e privado)
Instala o Tailscale no host e nos seus aparelhos; todos entram numa rede privada. Acessa pelo IP `100.x` do host, sem abrir nada pra internet pública. Com **Tailscale Serve** dá pra ter HTTPS automático.

### Opção B — Cloudflare Tunnel (URL pública com HTTPS)
Expõe `clip.seudominio.com` com HTTPS, sem abrir portas no roteador:

```yaml
# adicione ao docker-compose.yml
  cloudflared:
    image: cloudflare/cloudflared:latest
    restart: unless-stopped
    command: tunnel run
    environment:
      TUNNEL_TOKEN: "<seu-token-do-painel-cloudflare>"
```

No painel da Cloudflare, aponte o tunnel para `http://clip:8080`.

> Exposto à internet, **o PIN é obrigatório**. Cookies são marcados `Secure` automaticamente quando o acesso é HTTPS.

## Limitação honesta

Por ser web, o fluxo é **"abrir a página e colar/copiar"** — não é sincronização automática do clipboard do sistema operacional em segundo plano (isso exigiria app nativo em cada aparelho). Pra copiar-e-colar entre dispositivos sem instalar nada, esse é o caminho mais leve e prático.
