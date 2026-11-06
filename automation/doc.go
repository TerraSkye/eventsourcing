// Package automation runs background processors that work off a todo list.
//
// In Event Modeling, an automation is a processor that looks at a read model
// listing pending work (the todo list), does that work, and records the
// outcome by sending a command. The resulting event removes the item from
// the todo list, which closes the loop:
//
//	OrderPlaced ──► orders to ship (todo list) ──► ship-orders automation
//	     ▲                                                │
//	     └──────── OrderShipped ◄──── ShipOrder ◄─────────┘
//
// The todo list is an ordinary projection (see the projection package),
// usually a table. The automation never reacts to events directly; it asks
// the todo list what is pending. That keeps the automation stateless,
// makes stuck work visible with a query, and lets a crashed automation
// resume by simply asking again.
//
// # Writing an automation
//
// An automation needs two functions next to the todo list's projection: one
// that claims pending items, and one that does the work for one item.
//
//	// Claim reads pending items from the todo list and leases them, so that
//	// other instances skip them for a while.
//	func (p *OrdersToShip) Claim(ctx context.Context, max int, lease time.Duration) ([]ShippingTodo, error) {
//		rows, err := p.pool.Query(ctx, `
//			UPDATE orders_to_ship SET claimed_until = now() + $2
//			WHERE order_id IN (
//				SELECT order_id FROM orders_to_ship
//				WHERE claimed_until IS NULL OR claimed_until < now()
//				ORDER BY order_id
//				LIMIT $1
//				FOR UPDATE SKIP LOCKED)
//			RETURNING order_id, address`, max, lease)
//		if err != nil {
//			return nil, err
//		}
//		return pgx.CollectRows(rows, pgx.RowToStructByPos[ShippingTodo])
//	}
//
//	// ShipOrder does the work for one item and records the outcome.
//	func ShipOrder(bus eventsourcing.Dispatcher, carrier Carrier) automation.WorkFunc[ShippingTodo] {
//		return func(ctx context.Context, t ShippingTodo) error {
//			tracking, err := carrier.CreateShipment(ctx, t.OrderID, t.Address)
//			if err != nil {
//				return err
//			}
//			_, err = bus.Dispatch(ctx, ShipOrder{OrderID: t.OrderID, TrackingNo: tracking})
//			return err
//		}
//	}
//
// Wiring the todo list and the automation together:
//
//	ordersToShip := shipping.NewOrdersToShip(pool)
//	todoList := projection.NewRunner(shipping.NewProjection(),
//		eventStore, postgres.NewProjectionStore(pool))
//
//	ship := automation.NewRunner(automation.Definition[shipping.ShippingTodo]{
//		Name:       "ship-orders",
//		TodoList:   ordersToShip,
//		Projection: todoList,
//		Store:      postgres.NewAutomationStore(pool),
//		Work:       shipping.ShipOrder(commandBus, carrier),
//	})
//
// The Store keeps which instance holds which segment, and pauses: a
// postgres or sqlite store shares them between every instance on the
// database, memory.NewAutomationStore keeps them in one process.
//
//	go todoList.Run(ctx)
//	go ship.Run(ctx)
//
// # Waiting for the todo list
//
// An automation only claims work while its todo list is live. While the todo
// list is being rebuilt, it temporarily lists items that are already done
// further along in the log; an automation acting on them would repeat work
// that was finished long ago. The same goes for a todo list that is stalled
// on a bad event, or whose instance died: completed items would never
// disappear from it.
//
// The automation therefore checks its todo list, and anything listed in
// [Definition.WaitFor], before every claim, not only at start. Items claimed before a
// dependency stopped being live are finished normally.
//
// The todo list may run in another process or another program; pass a
// [projection.Probe] for it instead of a runner. See [Dependency].
//
// # Doing the work safely
//
// Work is delivered at least once. An item whose work failed, or whose
// automation crashed halfway, is claimed again once its lease expires. The
// work must therefore be safe to repeat:
//
//   - The command it sends should be idempotent: a command handler that
//     returns no events for work already recorded (an order already
//     shipped) makes a repeated command harmless.
//   - Calls to external systems should carry an idempotency key, typically
//     the item's ID, so that a retry after a crash does not create a
//     second shipment.
//
// There is no attempt limit and no dead letter: an item whose work keeps
// failing is retried once per lease for as long as it stays on the todo
// list. Nor is the work cancelled when the lease runs out; ctx is only
// cancelled when the automation stops. Work that fails for good should
// record that outcome as an event, so the todo list drops the item, and
// work that may hang should set its own deadline well below
// [Definition.Lease].
//
// Work starts with no chain, so the commands it sends start a new one.
// To continue the chain of the event the item comes from, keep that
// event's CorrelationID and EventID on the todo-list row and set them in
// Work, before sending the command:
//
//	ctx = eventsourcing.WithCorrelationID(ctx, item.CorrelationID)
//	ctx = eventsourcing.WithCausationID(ctx, item.EventID)
//
// See eventsourcing.Envelope.CorrelationID.
//
// # Segments, concurrency and order
//
// By default every instance of an automation claims from the whole todo
// list and runs its items one at a time: competing consumers, enough for
// work with no limits of its own. The list and the definition can add
// structure when the work has some:
//
//   - A list that implements [Segmented] is divided by a column the
//     projection writes: a shard number ([Shards], with [ShardKey]) or a
//     business key such as a tenant ([Query]). Each segment is leased to
//     one instance at a time through a [Store], so "at most K calls at
//     once to this provider" holds across every instance, and a [Deferred]
//     return from Work (a 429) pauses that segment, and only that segment,
//     for every instance. Segments with a Group round-robin across groups
//     before segments, so a tenant with forty segments and a tenant with
//     one get the same number of turns. The list claims every held
//     segment in one call and hands the items back grouped by segment.
//     Each segment's batch runs on its own: when it finishes, that segment
//     is claimed for again without waiting for slower segments, and a
//     deferred segment is given back, paused, once its batch is done.
//   - [Definition.Concurrency] is how many items of one segment run at
//     once.
//   - [Definition.OrderingKey] groups items that must run one at a time in
//     claim order, an order or an aggregate; different ordering keys fill
//     Concurrency. A failed item parks its ordering key for the rest of the
//     batch, so a later item of the same chain cannot overtake it.
//
// # Pausing and following
//
// Every instance publishes its [Status] to the [Store] and checks there
// whether the automation is paused. [Store.SetPaused] pauses it for every
// instance at once: each stops claiming at its next poll, lets the items
// it is working on finish, and gives its segments back; [Runner.RunOnce]
// returns at once while it is paused. [Store.LoadStatuses] and
// [Store.LoadSegments] read back what every instance published; the
// dashboard package shows them and offers the pause as a button.
//
// Scheduling is pluggable: [Definition.Scheduler] replaces the claiming
// and leasing with another source of keyed work, a broker or a queue,
// while the runner keeps dispatch, deadlines and outcomes. See
// [Scheduler].
//
// Leases narrow the window in which two instances work the same item;
// they do not close it. A call already sent to an external system when
// a lease is lost can still land after another instance has retried it.
// Idempotency on the outgoing call is the user's job; "leased" is not
// "exactly-once".
//
// # Running on a timer
//
// [Runner.Run] works until its context ends. [Runner.RunOnce] works until
// nothing is eligible, gives every segment back, and returns a
// [Summary]: the entry point for an automation that a cron job or a timer
// starts, say once a night.
//
// # Waking up
//
// An automation checks for work at every [Definition.PollInterval], and
// immediately when nudged. When its dependencies implement
// [Watcher], as a [projection.Runner] does, the automation is
// nudged whenever the todo list commits new events or becomes live, so
// new work is picked up within milliseconds.
//
// # Status
//
// [Runner.Status] reports a [Phase] (Waiting, Idle, Working, Stalled)
// and counts for this instance, in the same shape as a projection's
// status. A failing check or scheduler call stalls the automation with the same backoff as a projection,
// 0.5s growing to 30s, and it never gives up; a failing work call is
// counted, logged, and retried once its item's lease expires.
package automation
