// HTMX event handlers for stackr.

// Log HTMX errors to the console.
document.addEventListener('htmx:responseError', function(evt) {
    console.error('HTMX request failed:', evt.detail.xhr.status, evt.detail.xhr.statusText);
});

// Re-initialize components after HTMX swaps.
document.addEventListener('htmx:afterSwap', function(evt) {
    // Scroll to the first validation error if present.
    // An SSE outerHTML swap has no target.
    var firstError = evt.detail.target && evt.detail.target.querySelector('.field-error');
    if (firstError && firstError.textContent.trim() !== '') {
        firstError.scrollIntoView({ behavior: 'smooth', block: 'center' });
    }
});

// Handle HTMX send errors (network failures).
document.addEventListener('htmx:sendError', function(evt) {
    console.error('HTMX network error for:', evt.detail.elt);
});

// Re-validate a field while typing, but only once it is already showing an
// error. Pairs with hx-trigger="blur, hamr:revalidate" on the input. The
// condition lives here rather than in an hx-trigger [...] filter because htmx
// compiles those with Function(), which needs 'unsafe-eval' in the CSP.
// Fields are matched to their error span by the id convention in
// components/form: <input name="x"> <-> <span id="error-x" data-has-error>.
// data-hamr-watch="y" watches another field's error span instead (cross-field).
document.addEventListener('input', function(evt) {
    var el = evt.target;
    if (!el.name) return;
    var err = document.getElementById('error-' + (el.dataset.hamrWatch || el.name));
    if (!err || err.dataset.hasError !== 'true') return;
    clearTimeout(el._hamrRevalidate);
    el._hamrRevalidate = setTimeout(function() { htmx.trigger(el, 'hamr:revalidate'); }, 300);
});
