module.exports = {
    extends: [
        '@commitlint/config-conventional'
    ],
    rules: {
        'type-case': [2, 'always', 'start-case'],
        'type-enum': [
            2,
            'always',
            [
                'Feature',
                'Refactor',
                'Perf',
                'Fix',
                'Revert',
                'Docs',
                'Style',
                'Chore',
                'CI',
                'Build',
                'Test'
            ]
        ],
        'subject-case': [2, 'always', 'sentence-case'],
    },
};
