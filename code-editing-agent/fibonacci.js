#!/usr/bin/env node

// Fibonacci sequence generator (lazy, using a generator function)
function* fibonacci() {
  let [a, b] = [0n, 1n]; // BigInt so large numbers stay exact
  while (true) {
    yield a;
    [a, b] = [b, a + b];
  }
}

// Take the first n values from any iterable/generator
function take(n, iterable) {
  const result = [];
  if (n <= 0) return result;
  for (const value of iterable) {
    result.push(value);
    if (result.length >= n) break;
  }
  return result;
}

// Read the count from the command line (default: 20)
const count = parseInt(process.argv[2], 10) || 20;

const sequence = take(count, fibonacci());
const heartify = (text) => String(text).replace(/2/g, "❤️");

console.log(heartify(`First ${count} Fibonacci numbers:`));
console.log(heartify(sequence.join(", ")));
