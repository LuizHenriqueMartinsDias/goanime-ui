// GoAnime desktop: runs the goanime-ui server and shows its interface in a
// window of its own instead of a browser tab.
const { app, BrowserWindow, dialog, shell } = require('electron');
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

// The page keeps its preferences in localStorage, which belongs to the
// origin, so the port stays fixed instead of picking a free one per launch.
const PORT = Number(process.env.GOANIME_PORT) || 7777;
const APP_URL = `http://127.0.0.1:${PORT}`;
const BOOT_TIMEOUT_MS = 20000;

let server = null;
let serverLog = '';
let win = null;

if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on('second-instance', () => {
    if (!win) return;
    if (win.isMinimized()) win.restore();
    win.focus();
  });
  app.whenReady().then(start);
}

// serverBinary prefers the binary packaged next to the app, then the one
// `npm run build:server` drops in bin/, then a `go install`ed one.
function serverBinary() {
  const candidates = [
    path.join(process.resourcesPath, 'bin', 'goanime-ui'),
    path.join(__dirname, 'bin', 'goanime-ui'),
    path.join(os.homedir(), 'go', 'bin', 'goanime-ui'),
  ];
  return candidates.find((p) => fs.existsSync(p)) || 'goanime-ui';
}

async function isServing() {
  try {
    const res = await fetch(`${APP_URL}/api/sources`, { signal: AbortSignal.timeout(1500) });
    return res.ok;
  } catch {
    return false;
  }
}

async function start() {
  // A goanime-ui already running (the browser version) is reused as is.
  if (!(await isServing())) {
    try {
      await startServer();
    } catch (err) {
      dialog.showErrorBox('GoAnime', `Não foi possível iniciar o servidor do GoAnime.\n\n${err.message}`);
      app.quit();
      return;
    }
  }
  createWindow();
}

function startServer() {
  return new Promise((resolve, reject) => {
    server = spawn(serverBinary(), ['--no-open', '--port', String(PORT)], {
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    const keep = (chunk) => { serverLog = (serverLog + chunk).slice(-4000); };
    server.stdout.on('data', keep);
    server.stderr.on('data', keep);

    let booted = false;
    server.on('error', (err) => reject(err));
    server.on('exit', (code) => {
      server = null;
      if (!booted) {
        reject(new Error(serverLog.trim() || `o servidor saiu com código ${code}`));
        return;
      }
      if (!app.isQuitting) {
        dialog.showErrorBox('GoAnime', `O servidor do GoAnime parou.\n\n${serverLog.trim()}`);
        app.quit();
      }
    });

    const deadline = Date.now() + BOOT_TIMEOUT_MS;
    const poll = async () => {
      if (!server) return;
      if (await isServing()) {
        booted = true;
        resolve();
      } else if (Date.now() > deadline) {
        reject(new Error('o servidor não respondeu a tempo'));
      } else {
        setTimeout(poll, 150);
      }
    };
    poll();
  });
}

function createWindow() {
  win = new BrowserWindow({
    width: 1280,
    height: 800,
    minWidth: 760,
    minHeight: 520,
    title: 'GoAnime',
    icon: path.join(__dirname, 'build', 'icon.png'),
    backgroundColor: '#0D0E13', // --bg in app.css, so there is no white flash
    autoHideMenuBar: true,
    show: false,
    webPreferences: { contextIsolation: true, nodeIntegration: false, sandbox: true },
  });
  win.removeMenu();
  win.once('ready-to-show', () => win.show());
  win.on('closed', () => { win = null; });

  // Anything outside the UI opens in the system browser.
  const isApp = (url) => url === APP_URL || url.startsWith(`${APP_URL}/`);
  win.webContents.setWindowOpenHandler(({ url }) => {
    if (/^https?:/.test(url)) shell.openExternal(url);
    return { action: 'deny' };
  });
  win.webContents.on('will-navigate', (event, url) => {
    if (isApp(url)) return;
    event.preventDefault();
    if (/^https?:/.test(url)) shell.openExternal(url);
  });

  // F12 / Ctrl+Shift+I for devtools, Ctrl+R to reload: the menu that carried
  // these shortcuts is gone.
  win.webContents.on('before-input-event', (event, input) => {
    if (input.type !== 'keyDown') return;
    const key = input.key.toLowerCase();
    if (key === 'f12' || (input.control && input.shift && key === 'i')) {
      win.webContents.toggleDevTools();
      event.preventDefault();
    } else if ((input.control && key === 'r') || key === 'f5') {
      win.webContents.reload();
      event.preventDefault();
    }
  });

  win.loadURL(APP_URL);
}

app.on('window-all-closed', () => app.quit());

app.on('before-quit', () => {
  app.isQuitting = true;
  // SIGTERM lets goanime-ui run its cleanup (stop mpv, temp files).
  if (server) server.kill('SIGTERM');
});
