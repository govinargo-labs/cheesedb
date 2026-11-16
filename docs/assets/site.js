// Shared by every page: the Copy buttons.
document.querySelectorAll('[data-copy], [data-copy-target]').forEach(function (b) {
  b.addEventListener('click', function () {
    var text = b.dataset.copy || document.querySelector(b.dataset.copyTarget).textContent;
    var done = function () { var old = b.textContent; b.textContent = 'Copied'; setTimeout(function () { b.textContent = old; }, 1400); };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, function () {});
    }
  });
});
