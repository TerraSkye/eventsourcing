package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

// recentTasks is how many open tasks the tasks page lists.
const recentTasks = 25

// readModels is what the tasks page shows: both read models as they are in
// the database now.
type readModels struct {
	// Open is how many tasks task-list holds.
	Open int `json:"open"`
	// Recent is the open tasks task-list added last, newest first.
	Recent []openTask `json:"recent"`
	// Owners is tasks-by-owner, by owner name.
	Owners []ownerCount `json:"owners"`
}

type openTask struct {
	TaskID string `json:"taskId"`
	Owner  string `json:"owner"`
}

type ownerCount struct {
	Owner string `json:"owner"`
	Tasks int    `json:"tasks"`
}

// tasksHandler serves the tasks page at /tasks and the read models it polls
// at /tasks.json, so a rebuild started on the dashboard can be watched
// emptying and refilling them in another tab.
func tasksHandler(db *sql.DB) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(tasksPage))
	})
	mux.HandleFunc("GET /tasks.json", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		models, err := loadReadModels(ctx, db)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		_ = json.NewEncoder(w).Encode(models)
	})
	return mux
}

// loadReadModels reads both read models in one transaction, so the page
// never shows them halfway through a projection's batch.
func loadReadModels(ctx context.Context, db *sql.DB) (readModels, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return readModels{}, err
	}
	defer tx.Rollback()

	m := readModels{Recent: []openTask{}, Owners: []ownerCount{}}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM demo_open_tasks`).Scan(&m.Open); err != nil {
		return readModels{}, err
	}
	// rowid grows with every insert, so the highest are the newest.
	rows, err := tx.QueryContext(ctx, `SELECT task_id, owner FROM demo_open_tasks ORDER BY rowid DESC LIMIT ?`, recentTasks)
	if err != nil {
		return readModels{}, err
	}
	for rows.Next() {
		var t openTask
		if err := rows.Scan(&t.TaskID, &t.Owner); err != nil {
			rows.Close()
			return readModels{}, err
		}
		m.Recent = append(m.Recent, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return readModels{}, err
	}

	rows, err = tx.QueryContext(ctx, `SELECT owner, tasks FROM demo_tasks_by_owner ORDER BY owner`)
	if err != nil {
		return readModels{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var o ownerCount
		if err := rows.Scan(&o.Owner, &o.Tasks); err != nil {
			return readModels{}, err
		}
		m.Owners = append(m.Owners, o)
	}
	return m, rows.Err()
}

// tasksPage polls /tasks.json twice a second and shows both read models.
const tasksPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Tasks</title>
<style>
  :root {
    color-scheme: light;
    --paper: #eef2f5; --surface: #fbfcfd; --ink: #15202b; --muted: #5b6b7b;
    --rule: #cdd6df; --track: #dce3ea; --bar: #2f6fb3; --lost: #c03a35;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      color-scheme: dark;
      --paper: #18212a; --surface: #1f2a35; --ink: #e3eaf1; --muted: #91a0af;
      --rule: #2e3b48; --track: #26323e; --bar: #7fb0e6; --lost: #ee6a62;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; color: var(--ink); background: var(--paper);
    font: 15px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    font-variant-numeric: tabular-nums;
  }
  main { max-width: 64rem; margin: 0 auto; padding: 2.5rem 1.5rem 4rem; }
  header { display: flex; align-items: baseline; justify-content: space-between; gap: 1rem; flex-wrap: wrap;
    padding-bottom: 1.25rem; border-bottom: 2px solid var(--ink); }
  h1 { margin: 0; font-size: 1.75rem; font-weight: 650; }
  h2 { margin: 2rem 0 0.25rem; font-size: 1.25rem; font-weight: 620; }
  .note { color: var(--muted); font-size: 0.875rem; margin: 0 0 1rem; }
  #status { color: var(--muted); font-size: 0.875rem; }
  #status.lost { color: var(--lost); }
  .big { font-size: 3rem; font-weight: 650; line-height: 1.1; }
  .owners { display: grid; grid-template-columns: max-content 1fr max-content; gap: 0.375rem 1rem; align-items: center; }
  .track { height: 0.75rem; background: var(--track); border-radius: 2px; overflow: hidden; }
  .fill { height: 100%; background: var(--bar); transition: width 300ms ease-out; }
  .num { text-align: right; }
  table { border-collapse: collapse; font-size: 0.875rem; width: 100%; }
  th { text-align: left; font-weight: 500; color: var(--muted); padding: 0.375rem 1.5rem 0.375rem 0; border-bottom: 1px solid var(--rule); }
  td { padding: 0.375rem 1.5rem 0.375rem 0; border-bottom: 1px solid var(--rule); }
  td.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 0.8125rem; overflow-wrap: anywhere; }
  .empty { color: var(--muted); padding: 1rem 0; }
  a { color: var(--bar); }
  @media (prefers-reduced-motion: reduce) { .fill { transition: none; } }
</style>
</head>
<body>
<main>
  <header>
    <h1>Tasks</h1>
    <span id="status" role="status">Loading</span>
  </header>
  <p class="note">The read models, read from the database twice a second. Press Rebuild on the <a href="/">dashboard</a> to watch them empty and fill up again.</p>

  <h2>Open tasks</h2>
  <p class="note">task-list: tasks created and not yet completed.</p>
  <div class="big" id="open">–</div>

  <h2>Tasks by owner</h2>
  <p class="note">tasks-by-owner: every task each owner created, in eight segments.</p>
  <div class="owners" id="owners"></div>

  <h2>Newest open tasks</h2>
  <table>
    <thead><tr><th>Task</th><th>Owner</th></tr></thead>
    <tbody id="recent"></tbody>
  </table>
</main>
<script>
(() => {
  "use strict";
  const num = new Intl.NumberFormat();
  const status = document.getElementById("status");

  function el(tag, cls, text) {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  function render(m) {
    document.getElementById("open").textContent = num.format(m.open);

    const owners = document.getElementById("owners");
    if (!m.owners.length) {
      owners.replaceChildren(el("div", "empty", "No tasks counted."));
    } else {
      const most = Math.max(...m.owners.map(o => o.tasks), 1);
      owners.replaceChildren(...m.owners.flatMap(o => {
        const fill = el("div", "fill");
        fill.style.width = (o.tasks / most * 100) + "%";
        const track = el("div", "track");
        track.append(fill);
        return [el("div", "", o.owner), track, el("div", "num", num.format(o.tasks))];
      }));
    }

    const recent = document.getElementById("recent");
    if (!m.recent.length) {
      const td = el("td", "empty", "No open tasks.");
      td.colSpan = 2;
      const tr = el("tr");
      tr.append(td);
      recent.replaceChildren(tr);
    } else {
      recent.replaceChildren(...m.recent.map(t => {
        const tr = el("tr");
        tr.append(el("td", "mono", t.taskId), el("td", "", t.owner));
        return tr;
      }));
    }
  }

  async function poll() {
    try {
      const res = await fetch("tasks.json", { cache: "no-store" });
      if (!res.ok) throw new Error(await res.text());
      render(await res.json());
      status.className = "";
      status.textContent = "Updated " + new Date().toLocaleTimeString();
    } catch (err) {
      status.className = "lost";
      status.textContent = "Could not read the tasks: " + err.message;
    }
    setTimeout(poll, 500);
  }
  poll();
})();
</script>
</body>
</html>
`
