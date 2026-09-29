/// <reference types="vitest/config" />
import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [
		sveltekit({
			compilerOptions: {
				// Force runes mode for the project, except for libraries. Can be removed in svelte 6.
				runes: ({ filename }) =>
					filename.split(/[/\\]/).includes('node_modules') ? undefined : true
			},

			// A single-page app: the Go server serves build/ and falls back to
			// index.html (LUMORA_STATIC_DIR).
			adapter: adapter({ fallback: 'index.html' })
		})
	],
	server: {
		// Backend runs on :8080 in dev (see Makefile).
		proxy: {
			'/healthz': 'http://localhost:8080',
			'/api': 'http://localhost:8080',
			'/auth': 'http://localhost:8080',
			'/ws': { target: 'ws://localhost:8080', ws: true }
		}
	},
	test: {
		include: ['src/**/*.test.ts']
	}
});
