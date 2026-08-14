import { defineConfig } from 'vite';
import preact from '@preact/preset-vite';

// The build lands inside internal/server because //go:embed cannot reach
// outside its own package directory. The Go binary embeds that directory, so
// this output is the shipped UI.
export default defineConfig({
  plugins: [preact()],

  // Relative asset paths, so the page works wherever it is mounted.
  base: './',

  build: {
    outDir: '../internal/server/dist',
    // The directory holds a committed .gitkeep so that `go build` works on a
    // fresh clone before anyone has run `make web`. Emptying it would delete
    // that and break the embed. Asset names are fixed below, so nothing goes
    // stale anyway.
    emptyOutDir: false,

    // One screen should not need code splitting, and a single file is easier
    // to reason about when it is being embedded in a binary.
    rollupOptions: {
      output: {
        entryFileNames: 'assets/[name].js',
        chunkFileNames: 'assets/[name].js',
        assetFileNames: 'assets/[name].[ext]',
      },
    },
  },

  server: {
    // `npm run dev` proxies to a `loupe serve` on the default port, which is
    // what CONTRIBUTING.md tells frontend contributors to expect.
    proxy: {
      '/api': 'http://127.0.0.1:7717',
    },
  },
});
