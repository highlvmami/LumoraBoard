<script lang="ts" module>
	export type Tool = 'select' | 'hand' | 'pen' | 'rect' | 'ellipse' | 'arrow' | 'text' | 'eraser';

	export type RemoteCursor = { id: string; name: string; color: string; x: number; y: number };
</script>

<script lang="ts">
	import { onMount, tick } from 'svelte';
	import type { BoardObject, Op, Point } from './board';
	import { boxFrom, TEXT_LINE, TEXT_SIZE, topmostAt } from './geometry';
	import { throttle } from './presence';
	import { draw, FONT, measureText } from './render';
	import { identity, panBy, toScreen, toWorld, zoomAt, type Viewport } from './viewport';

	type Props = {
		/** Board in paint order, including this client's pending ops. */
		objects: BoardObject[];
		cursors: RemoteCursor[];
		clientId: string;
		/** False while disconnected: the board is visible but read-only. */
		editable: boolean;
		onop: (op: Op) => void;
		/** World position of the local pointer, or null when it left. */
		oncursor: (p: Point | null) => void;
	};
	let { objects, cursors, clientId, editable, onop, oncursor }: Props = $props();

	const TOOLS: { id: Tool; label: string; key: string; icon: string }[] = [
		{ id: 'select', label: 'Select', key: 'v', icon: 'M5 3l14 8-6 2-3 6z' },
		{ id: 'hand', label: 'Pan', key: 'h', icon: 'M8 13V5a1.5 1.5 0 013 0v6m0-1V4a1.5 1.5 0 013 0v7m0-5a1.5 1.5 0 013 0v7c0 4-2.5 7-6 7s-5-1.5-7-5l-1.5-3a1.5 1.5 0 012.6-1.5L8 13' },
		{ id: 'pen', label: 'Pen', key: 'p', icon: 'M4 20l4-1 11-11-3-3L5 16zM14 6l3 3' },
		{ id: 'rect', label: 'Rectangle', key: 'r', icon: 'M4 6h16v12H4z' },
		{ id: 'ellipse', label: 'Ellipse', key: 'o', icon: 'M12 5c4.4 0 8 3.1 8 7s-3.6 7-8 7-8-3.1-8-7 3.6-7 8-7z' },
		{ id: 'arrow', label: 'Arrow', key: 'a', icon: 'M5 19L19 5m0 0h-8m8 0v8' },
		{ id: 'text', label: 'Text', key: 't', icon: 'M5 6V4h14v2M12 4v16m-3 0h6' },
		{ id: 'eraser', label: 'Eraser', key: 'e', icon: 'M7 20h13M4 15l9-9 6 6-7 7H8z' }
	];
	const COLORS = ['#1f2937', '#e11d48', '#2563eb', '#16a34a', '#d97706', '#7c3aed'];
	const WIDTHS = [2, 4, 8];

	let tool = $state<Tool>('pen');
	let color = $state(COLORS[0]);
	let width = $state(WIDTHS[1]);
	let view = $state<Viewport>(identity());
	let selected = $state('');
	let draft = $state<BoardObject | null>(null);
	let moving = $state<{ id: string; x: number; y: number } | null>(null);
	let spaceHeld = $state(false);

	/** In-place text editor, in world coordinates. */
	let editor = $state<{ id: string | null; x: number; y: number; text: string } | null>(null);
	let textarea = $state<HTMLTextAreaElement>();

	let container: HTMLDivElement;
	let canvas: HTMLCanvasElement;
	let ctx: CanvasRenderingContext2D | null = null;
	let size = $state({ width: 0, height: 0, dpr: 1 });

	// ---- rendering -------------------------------------------------------

	let frame = 0;
	function scheduleDraw() {
		if (frame || !ctx) return;
		frame = requestAnimationFrame(() => {
			frame = 0;
			if (ctx) draw(ctx, objects, view, size, { selected, draft, moving, hidden: editor?.id ?? undefined });
		});
	}

	$effect(() => {
		// Track everything draw() reads.
		void [objects, view, size, selected, draft, moving, editor?.id];
		scheduleDraw();
	});

	onMount(() => {
		ctx = canvas.getContext('2d');
		const resize = () => {
			const dpr = window.devicePixelRatio || 1;
			const { clientWidth: w, clientHeight: h } = container;
			canvas.width = Math.round(w * dpr);
			canvas.height = Math.round(h * dpr);
			size = { width: w, height: h, dpr };
		};
		const ro = new ResizeObserver(resize);
		ro.observe(container);
		resize();

		// Registered by hand: wheel must be non-passive to stop the page
		// from scrolling or zooming along with the board.
		const onWheel = (e: WheelEvent) => {
			e.preventDefault();
			const at = local(e);
			if (e.ctrlKey || e.metaKey) {
				// Pinch on a trackpad arrives as ctrl+wheel.
				view = zoomAt(view, at, Math.exp(-e.deltaY * 0.01));
			} else {
				view = panBy(view, -e.deltaX, -e.deltaY);
			}
		};
		canvas.addEventListener('wheel', onWheel, { passive: false });
		return () => {
			ro.disconnect();
			canvas.removeEventListener('wheel', onWheel);
			cancelAnimationFrame(frame);
		};
	});

	// ---- helpers ---------------------------------------------------------

	function local(e: { clientX: number; clientY: number }): Point {
		const r = canvas.getBoundingClientRect();
		return { x: e.clientX - r.left, y: e.clientY - r.top };
	}

	let counter = 0;
	function newId(): string {
		counter += 1;
		return `${(clientId || 'x').slice(0, 8)}-${Date.now().toString(36)}${counter.toString(36)}`;
	}

	function nextZ(): number {
		return objects.length ? objects[objects.length - 1].z + 1 : 0;
	}

	/** Hit tolerance: about 6 screen pixels whatever the zoom. */
	const tolerance = () => 6 / view.scale;

	// ---- gestures --------------------------------------------------------

	type Gesture =
		| { type: 'pan'; last: Point }
		| { type: 'pinch'; dist: number; mid: Point }
		| { type: 'pen'; id: string; origin: Point; last: Point; count: number }
		| { type: 'shape'; kind: 'rect' | 'ellipse' | 'arrow'; start: Point }
		| { type: 'move'; id: string; start: Point; orig: Point }
		| { type: 'erase'; erased: Set<string> };

	const pointers = new Map<number, Point>();
	let gesture: Gesture | null = null;

	// Stroke points stream to the room in small batches instead of one op
	// per pointer event: fewer messages, same live feel for everyone.
	let penBuffer: Point[] = [];
	const MAX_STROKE_POINTS = 10_000;
	const flushPen = throttle((id: string) => {
		while (penBuffer.length) {
			onop({ kind: 'append', id, points: penBuffer.splice(0, 512) });
		}
	}, 50);

	const sendMove = throttle((id: string, x: number, y: number) => onop({ kind: 'update', id, patch: { x, y } }), 50);

	function pinchState(): { dist: number; mid: Point } {
		const [a, b] = [...pointers.values()];
		return { dist: Math.hypot(a.x - b.x, a.y - b.y), mid: { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 } };
	}

	function onpointerdown(e: PointerEvent) {
		if (editor) commitText();
		canvas.setPointerCapture(e.pointerId);
		const p = local(e);
		pointers.set(e.pointerId, p);

		if (pointers.size === 2) {
			// A second finger turns whatever was happening into a pinch.
			// The first finger has usually started a stroke by then; if it
			// is barely a dot, take it back.
			const g = gesture;
			endGesture();
			if (g?.type === 'pen' && g.count < 4) onop({ kind: 'delete', id: g.id });
			gesture = { type: 'pinch', ...pinchState() };
			return;
		}
		if (pointers.size > 2) return;

		const w = toWorld(view, p);
		if (tool === 'hand' || e.button === 1 || spaceHeld || !editable) {
			gesture = { type: 'pan', last: p };
			return;
		}
		if (e.button !== 0) return;

		switch (tool) {
			case 'pen': {
				const id = newId();
				onop({
					kind: 'add',
					id,
					object: { id, kind: 'stroke', x: w.x, y: w.y, points: [{ x: 0, y: 0 }], color, strokeWidth: width, z: nextZ() }
				});
				gesture = { type: 'pen', id, origin: w, last: { x: 0, y: 0 }, count: 1 };
				break;
			}
			case 'rect':
			case 'ellipse':
			case 'arrow':
				gesture = { type: 'shape', kind: tool, start: w };
				break;
			case 'text': {
				// Keep the browser from moving focus to the page on the
				// mousedown that follows, which would blur the new editor.
				e.preventDefault();
				const hit = topmostAt(objects, w, tolerance());
				if (hit?.kind === 'text') openEditor(hit);
				else openEditor(null, w);
				break;
			}
			case 'eraser':
				gesture = { type: 'erase', erased: new Set() };
				erase(w);
				break;
			case 'select': {
				const hit = topmostAt(objects, w, tolerance());
				if (hit) {
					selected = hit.id;
					gesture = { type: 'move', id: hit.id, start: w, orig: { x: hit.x, y: hit.y } };
				} else {
					selected = '';
					gesture = { type: 'pan', last: p };
				}
				break;
			}
		}
	}

	function onpointermove(e: PointerEvent) {
		const p = local(e);
		if (pointers.has(e.pointerId)) pointers.set(e.pointerId, p);
		if (gesture?.type !== 'pinch') oncursor(toWorld(view, p));
		if (!gesture) return;

		switch (gesture.type) {
			case 'pan':
				view = panBy(view, p.x - gesture.last.x, p.y - gesture.last.y);
				gesture.last = p;
				break;
			case 'pinch': {
				if (pointers.size < 2) break;
				const next = pinchState();
				view = zoomAt(panBy(view, next.mid.x - gesture.mid.x, next.mid.y - gesture.mid.y), next.mid, next.dist / gesture.dist);
				gesture = { type: 'pinch', ...next };
				break;
			}
			case 'pen': {
				const g = gesture;
				const events = e.getCoalescedEvents?.() ?? [e];
				const minDist = 1.5 / view.scale;
				for (const ev of events.length ? events : [e]) {
					if (g.count >= MAX_STROKE_POINTS) break;
					const w = toWorld(view, local(ev));
					const rel = { x: w.x - g.origin.x, y: w.y - g.origin.y };
					if (Math.hypot(rel.x - g.last.x, rel.y - g.last.y) < minDist) continue;
					penBuffer.push(rel);
					g.last = rel;
					g.count += 1;
				}
				if (penBuffer.length) flushPen.call(g.id);
				break;
			}
			case 'shape':
				draft = shapeFrom(gesture.kind, gesture.start, toWorld(view, p), 'draft');
				break;
			case 'move': {
				const w = toWorld(view, p);
				const dx = w.x - gesture.start.x;
				const dy = w.y - gesture.start.y;
				// The local drag is drawn every frame; the room gets a
				// throttled stream of positions.
				moving = { id: gesture.id, x: gesture.orig.x + dx, y: gesture.orig.y + dy };
				sendMove.call(gesture.id, moving.x, moving.y);
				break;
			}
			case 'erase':
				erase(toWorld(view, p));
				break;
		}
	}

	function onpointerup(e: PointerEvent) {
		pointers.delete(e.pointerId);
		if (gesture?.type === 'pinch') {
			// Lifting one finger ends the pinch; don't start drawing with
			// the one that is left.
			if (pointers.size === 0) gesture = null;
			return;
		}
		endGesture(toWorld(view, local(e)));
	}

	function onpointerleave(e: PointerEvent) {
		if (e.pointerType === 'mouse') oncursor(null);
	}

	/** Finishes the current gesture, committing whatever it produced. */
	function endGesture(at?: Point) {
		const g = gesture;
		gesture = null;
		if (!g) return;
		switch (g.type) {
			case 'pen':
				flushPen.flush();
				break;
			case 'shape': {
				const end = at ?? g.start;
				draft = null;
				if (Math.hypot(end.x - g.start.x, end.y - g.start.y) * view.scale < 4) break;
				const id = newId();
				const { version: _v, createdBy: _c, ...object } = shapeFrom(g.kind, g.start, end, id);
				onop({ kind: 'add', id, object });
				break;
			}
			case 'move':
				sendMove.flush();
				moving = null;
				break;
		}
	}

	function shapeFrom(kind: 'rect' | 'ellipse' | 'arrow', a: Point, b: Point, id: string): BoardObject {
		const base = { id, color, strokeWidth: width, z: nextZ(), version: 0 };
		if (kind === 'arrow') {
			return { ...base, kind, x: a.x, y: a.y, points: [{ x: 0, y: 0 }, { x: b.x - a.x, y: b.y - a.y }] };
		}
		const box = boxFrom(a, b);
		return { ...base, kind, x: box.x, y: box.y, w: box.w, h: box.h };
	}

	function erase(w: Point) {
		if (gesture?.type !== 'erase') return;
		const hit = topmostAt(objects, w, tolerance());
		if (!hit || gesture.erased.has(hit.id)) return;
		gesture.erased.add(hit.id);
		if (selected === hit.id) selected = '';
		onop({ kind: 'delete', id: hit.id });
	}

	// ---- text ------------------------------------------------------------

	function openEditor(existing: BoardObject | null, at?: Point) {
		editor = existing
			? { id: existing.id, x: existing.x, y: existing.y, text: existing.text ?? '' }
			: { id: null, x: at!.x, y: at!.y - (TEXT_SIZE * TEXT_LINE) / 2, text: '' };
		tick().then(() => textarea?.focus());
	}

	function commitText() {
		const ed = editor;
		editor = null;
		if (!ed || !ctx) return;
		const text = ed.text.replace(/\s+$/, '');
		if (ed.id === null) {
			if (!text) return;
			const id = newId();
			const { w, h } = measureText(ctx, text);
			onop({ kind: 'add', id, object: { id, kind: 'text', x: ed.x, y: ed.y, w, h, text, color, z: nextZ() } });
			return;
		}
		if (!text) {
			onop({ kind: 'delete', id: ed.id });
			return;
		}
		const { w, h } = measureText(ctx, text);
		onop({ kind: 'update', id: ed.id, patch: { text, w, h } });
	}

	function oneditorkey(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			e.preventDefault();
			editor = null;
		} else if (e.key === 'Enter' && !e.shiftKey) {
			e.preventDefault();
			commitText();
		}
	}

	function ondblclick(e: MouseEvent) {
		if (!editable || (tool !== 'select' && tool !== 'text')) return;
		const hit = topmostAt(objects, toWorld(view, local(e)), tolerance());
		if (hit?.kind === 'text') {
			gesture = null;
			moving = null;
			openEditor(hit);
		}
	}

	// ---- keyboard --------------------------------------------------------

	function typing(e: KeyboardEvent) {
		const t = e.target as HTMLElement | null;
		return !!t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable);
	}

	function onkeydown(e: KeyboardEvent) {
		if (typing(e) || e.ctrlKey || e.metaKey || e.altKey) return;
		if (e.key === ' ') {
			spaceHeld = true;
			e.preventDefault();
			return;
		}
		if ((e.key === 'Delete' || e.key === 'Backspace') && selected && editable) {
			onop({ kind: 'delete', id: selected });
			selected = '';
			return;
		}
		if (e.key === 'Escape') {
			selected = '';
			return;
		}
		const t = TOOLS.find((x) => x.key === e.key.toLowerCase());
		if (t) tool = t.id;
	}

	function onkeyup(e: KeyboardEvent) {
		if (e.key === ' ') spaceHeld = false;
	}

	// Drop the selection when someone else deletes the object.
	$effect(() => {
		if (selected && !objects.some((o) => o.id === selected)) selected = '';
	});

	function zoomBy(factor: number) {
		view = zoomAt(view, { x: size.width / 2, y: size.height / 2 }, factor);
	}

	let cursorStyle = $derived(
		spaceHeld || tool === 'hand' ? 'grab' : tool === 'select' ? 'default' : tool === 'text' ? 'text' : 'crosshair'
	);
	let editorScreen = $derived(editor ? toScreen(view, editor) : null);
</script>

<svelte:window {onkeydown} {onkeyup} onblur={() => (spaceHeld = false)} />

<div class="board" bind:this={container}>
	<canvas
		bind:this={canvas}
		style:cursor={cursorStyle}
		data-testid="board"
		{onpointerdown}
		{onpointermove}
		{onpointerup}
		onpointercancel={onpointerup}
		{onpointerleave}
		{ondblclick}
	></canvas>

	{#each cursors as c (c.id)}
		{@const s = toScreen(view, c)}
		{#if s.x > -40 && s.y > -40 && s.x < size.width + 40 && s.y < size.height + 40}
			<div class="cursor" data-cursor={c.id} style:transform="translate({s.x}px, {s.y}px)" style:--c={c.color}>
				<svg width="18" height="18" viewBox="0 0 18 18" aria-hidden="true"
					><path d="M1 1l6 15 2.2-6.3L16 7.5z" fill="var(--c)" stroke="white" stroke-width="1.2" stroke-linejoin="round" /></svg
				>
				<span>{c.name}</span>
			</div>
		{/if}
	{/each}

	{#if editor && editorScreen}
		<textarea
			bind:this={textarea}
			bind:value={editor.text}
			class="editor"
			maxlength="4000"
			rows="1"
			placeholder="Type…"
			style:left="{editorScreen.x}px"
			style:top="{editorScreen.y}px"
			style:font="{TEXT_SIZE * view.scale}px/{TEXT_LINE} {FONT}"
			style:color
			onkeydown={oneditorkey}
			onblur={commitText}
		></textarea>
	{/if}

	<div class="toolbar" role="toolbar" aria-label="Tools">
		{#each TOOLS as t (t.id)}
			<button
				type="button"
				class:active={tool === t.id}
				aria-pressed={tool === t.id}
				title="{t.label} ({t.key.toUpperCase()})"
				aria-label={t.label}
				onclick={() => (tool = t.id)}
			>
				<svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true"
					><path d={t.icon} fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" /></svg
				>
			</button>
		{/each}
		<span class="sep"></span>
		{#each COLORS as c (c)}
			<button
				type="button"
				class="swatch"
				class:active={color === c}
				aria-label="Color {c}"
				aria-pressed={color === c}
				style:--c={c}
				onclick={() => (color = c)}
			></button>
		{/each}
		<span class="sep"></span>
		{#each WIDTHS as w (w)}
			<button
				type="button"
				class:active={width === w}
				aria-label="Width {w}"
				aria-pressed={width === w}
				onclick={() => (width = w)}
			>
				<span class="dot" style:width="{w + 2}px" style:height="{w + 2}px"></span>
			</button>
		{/each}
	</div>

	<div class="zoom">
		<button type="button" aria-label="Zoom out" onclick={() => zoomBy(1 / 1.25)}>−</button>
		<button type="button" class="pct" title="Reset view" onclick={() => (view = identity())}>
			{Math.round(view.scale * 100)}%
		</button>
		<button type="button" aria-label="Zoom in" onclick={() => zoomBy(1.25)}>+</button>
	</div>
</div>

<style>
	.board {
		position: relative;
		width: 100%;
		height: 100%;
		overflow: hidden;
		background: #fafafa;
		user-select: none;
	}
	canvas {
		position: absolute;
		inset: 0;
		width: 100%;
		height: 100%;
		touch-action: none;
		display: block;
	}
	.cursor {
		position: absolute;
		left: 0;
		top: 0;
		pointer-events: none;
		/* Remote positions arrive at ~25 Hz; a short glide hides the steps. */
		transition: transform 80ms linear;
		will-change: transform;
	}
	.cursor svg {
		display: block;
		filter: drop-shadow(0 1px 1px rgba(0, 0, 0, 0.25));
	}
	.cursor span {
		position: absolute;
		left: 14px;
		top: 16px;
		background: var(--c);
		color: white;
		font: 600 11px/1 system-ui, sans-serif;
		padding: 3px 6px;
		border-radius: 4px;
		white-space: nowrap;
	}
	.editor {
		position: absolute;
		min-width: 4rem;
		field-sizing: content;
		padding: 0;
		margin: 0;
		border: 1px dashed #2563eb;
		background: rgba(255, 255, 255, 0.85);
		outline: none;
		resize: none;
		overflow: hidden;
		white-space: pre;
	}
	.toolbar,
	.zoom {
		position: absolute;
		display: flex;
		align-items: center;
		gap: 2px;
		padding: 4px;
		background: white;
		border: 1px solid #e4e4e7;
		border-radius: 10px;
		box-shadow: 0 4px 16px rgba(0, 0, 0, 0.08);
	}
	.toolbar {
		top: 12px;
		left: 50%;
		transform: translateX(-50%);
		max-width: calc(100% - 24px);
		overflow-x: auto;
	}
	.zoom {
		bottom: 12px;
		right: 12px;
	}
	button {
		flex: none;
		display: grid;
		place-items: center;
		min-width: 34px;
		height: 34px;
		border: 0;
		border-radius: 7px;
		background: transparent;
		color: #3f3f46;
		cursor: pointer;
		font: 500 14px system-ui, sans-serif;
	}
	button:hover {
		background: #f4f4f5;
	}
	button.active {
		background: #e0e7ff;
		color: #3730a3;
	}
	.pct {
		min-width: 52px;
		font-variant-numeric: tabular-nums;
	}
	.swatch::after {
		content: '';
		width: 18px;
		height: 18px;
		border-radius: 50%;
		background: var(--c);
	}
	.swatch.active::after {
		box-shadow: 0 0 0 2px white, 0 0 0 4px var(--c);
	}
	.dot {
		display: block;
		border-radius: 50%;
		background: currentColor;
	}
	.sep {
		flex: none;
		width: 1px;
		height: 22px;
		margin: 0 4px;
		background: #e4e4e7;
	}
</style>
