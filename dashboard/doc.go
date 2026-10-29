// Package dashboard serves a web page that shows how a program's projections
// are doing, live: how far each segment has processed the log, how far it is
// behind, how fast it is catching up, and why a stalled segment stalled. It
// can also rebuild a projection, to show a replay as it happens.
//
// The page reads the status every runner publishes to the projection
// [projection.Store], so it shows projections wherever they run. Pass the
// runners this program runs with [WithRunners], and the names of
// projections other programs run with [WithProjections]:
//
//	d := dashboard.New(projectionStore,
//		dashboard.WithRunners(taskList, shipments),
//		dashboard.WithProjections("invoices"), // run by another service
//	)
//	mux.Handle("/dashboard/", http.StripPrefix("/dashboard", d))
//
// The handler serves the page at its root, so mount it on a path ending in
// a slash. It has no authentication of its own: wrap it in the program's
// own auth middleware before exposing it. It answers only requests for
// localhost or an IP address; name the host it is reached under with
// [WithAllowedHosts], as in dashboard.WithAllowedHosts("ops.example.com").
//
// # Rebuilding
//
// [WithReset] adds a rebuild button to every projection shown, including
// ones another program runs. It records a request with
// [projection.Store.RequestRebuild], which the runner holding segment 0
// serves between two batches, wherever it runs, so a dashboard in a web
// process can rebuild a projection a worker runs. The page shows the
// request as pending until then. The rebuild wipes the read model and
// replays the log, so queries see an empty or partial read model until
// the projection is live again. A projection with no Reset cannot be
// rebuilt: the button is hidden for one this program runs, and for one
// run elsewhere the request stays pending and its runner logs why.
//
// The rebuild endpoint only accepts requests from the dashboard's own page
// (checked through the Origin and Sec-Fetch-Site headers), so another site
// cannot trigger one from a visitor's browser. The check on the Host header
// keeps a site that rebinds its own name to the dashboard's address from
// passing for it.
//
// # Updates
//
// The page receives updates as server-sent events. While at least one page
// is open, the dashboard reads every projection's status once per interval
// (see [WithInterval]), however many pages are open, and stops reading when
// the last one closes. A runner given with [WithRunners] also pushes an
// update as soon as it commits or changes phase.
package dashboard
