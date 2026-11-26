// Filter successful rollback pairs by immutable execution IDs. Failed attempts
// and failed rollback attempts always remain visible.
export function filterExecutionHistory(items, hideRollback) {
  if (!hideRollback) return [...items];
  const pairs = new Set(items
    .filter((item) => item.operation === "rollback" && item.status === "success" && item.source_execution_id)
    .map((item) => item.source_execution_id));
  const rolledBackExecutions = new Set(items
    .filter((item) => pairs.has(item.execution_id) && item.status === "success" && ["apply", "forward"].includes(item.operation))
    .map((item) => item.execution_id));
  return items.filter((item) => {
    if (item.operation === "rollback" && item.status === "success" && pairs.has(item.source_execution_id)) return false;
    return !(item.status === "success" && rolledBackExecutions.has(item.execution_id));
  });
}
