// Progressive enhancement only; every page works without JS.
document.addEventListener("click", async (e) => {
  const copy = e.target.closest("[data-copy]");
  if (copy) {
    try {
      await navigator.clipboard.writeText(copy.dataset.copy);
      const label = copy.textContent;
      copy.textContent = "Copied";
      setTimeout(() => (copy.textContent = label), 1500);
    } catch {
      window.prompt("Copy this link:", copy.dataset.copy);
    }
    return;
  }
  if (e.target.closest("[data-print]")) window.print();
});
