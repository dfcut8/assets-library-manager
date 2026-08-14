(() => {
  const dialog = document.querySelector("[data-preview-dialog]");
  const openButton = document.querySelector("[data-preview-open]");
  const closeButton = document.querySelector("[data-preview-close]");

  if (!(dialog instanceof HTMLDialogElement) || !openButton || !closeButton) {
    return;
  }

  openButton.addEventListener("click", () => dialog.showModal());
  closeButton.addEventListener("click", () => dialog.close());
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog) {
      dialog.close();
    }
  });
  dialog.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      event.preventDefault();
      dialog.close();
    }
  });
})();
