# GoAnime Desktop

Wrapper Electron do `goanime-ui`: sobe o servidor local em `127.0.0.1:7777`
e mostra a interface numa janela própria. Se um `goanime-ui` já estiver
rodando nessa porta, o app usa ele. Ao fechar a janela, o servidor que o app
iniciou recebe SIGTERM e faz a limpeza normal (mpv, arquivos temporários).

```sh
npm install
npm start        # compila ../cmd/goanime-ui em bin/ e abre o app
npm run dist     # dist/: linux-unpacked/, GoAnime-*.AppImage e .deb
```

`GOANIME_PORT` troca a porta. Requer Go, Node e `mpv` no PATH.
